package s3_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/middleware"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// partGauge is client middleware that holds each UploadPart for a moment
// and records the most parts it saw in flight at once.
type partGauge struct {
	mu       sync.Mutex
	inFlight int
	max      int
}

func (g *partGauge) option(stack *middleware.Stack) error {
	return stack.Finalize.Add(middleware.FinalizeMiddlewareFunc("partGauge", func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
		if awsmiddleware.GetOperationName(ctx) != "UploadPart" {
			return next.HandleFinalize(ctx, in)
		}
		g.mu.Lock()
		g.inFlight++
		g.max = max(g.max, g.inFlight)
		g.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		defer func() { g.mu.Lock(); g.inFlight--; g.mu.Unlock() }()
		return next.HandleFinalize(ctx, in)
	}), middleware.After)
}

// TestConcurrency proves Config.Concurrency caps the parts of one upload
// in flight, where the SDK default sends several at once, and that a
// negative value is refused.
func TestConcurrency(t *testing.T) {
	const partSize = 5 << 20 // S3's minimum part size
	// Four parts: above the transfer manager's 16 MiB multipart threshold.
	body := strings.Repeat("x", 4*partSize)
	for _, tc := range []struct {
		name        string
		concurrency int
		check       func(int) bool
	}{{"one", 1, func(n int) bool { return n == 1 }}, {"default", 0, func(n int) bool { return n > 1 }}} {
		t.Run(tc.name, func(t *testing.T) {
			client, bucket := fakeS3(t)
			var g partGauge
			gauged := s3.New(client.Options(), func(o *s3.Options) { o.APIOptions = append(o.APIOptions, g.option) })
			drv, err := fss3.New(fss3.Config{Client: gauged, Bucket: bucket, PartSize: partSize, Concurrency: tc.concurrency})
			if err != nil {
				t.Fatal(err)
			}
			d, _ := filesystem.NewDisk("s3", drv)
			// A reader of unknown length goes through the transfer
			// manager's multipart upload, as an upload stream does.
			if _, err := d.Write(t.Context(), "big", io.MultiReader(strings.NewReader(body)), filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			if !tc.check(g.max) {
				t.Fatalf("at most %d parts in flight", g.max)
			}
		})
	}
	client, bucket := fakeS3(t)
	_, err := fss3.New(fss3.Config{Client: client, Bucket: bucket, Concurrency: -1})
	wantCode(t, err, filesystem.CodeInvalidArgument)
}

// TestRequestHeaders proves ExpectedBucketOwner and RequestPayer go on
// every request and into a presigned URL's signed headers, that the
// default sends neither, and that malformed values are refused.
func TestRequestHeaders(t *testing.T) {
	ctx := t.Context()
	const owner = "111122223333"
	d, p := newDisk(t, fss3.Config{ExpectedBucketOwner: owner, RequestPayer: types.RequestPayerRequester})
	// Requests before this point (the test bucket's creation) are not the
	// driver's.
	before := map[string]int{http.MethodPut: len(p.sent(http.MethodPut, "")), http.MethodGet: len(p.sent(http.MethodGet, ""))}
	if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadAll(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ListPage(ctx, "", filesystem.ListOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{http.MethodPut, http.MethodGet} {
		for _, r := range p.sent(m, "")[before[m]:] {
			if r.header.Get("X-Amz-Expected-Bucket-Owner") != owner || r.header.Get("X-Amz-Request-Payer") != "requester" {
				t.Fatalf("%s %s?%s sent %v", m, r.path, r.query, r.header)
			}
		}
	}
	req, err := d.SignedURL(ctx, "k", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	// SigV4 presigning moves x-amz-* headers into the signed query.
	u, _ := url.Parse(req.URL)
	if q := u.Query(); q.Get("x-amz-expected-bucket-owner") != owner || q.Get("x-amz-request-payer") != "requester" || !strings.Contains(q.Get("X-Amz-SignedHeaders"), "host") {
		t.Errorf("presigned URL = %s", req.URL)
	}

	plain, pp := newDisk(t, fss3.Config{})
	if _, err := plain.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, r := range pp.sent(http.MethodPut, "") {
		if r.header.Get("X-Amz-Expected-Bucket-Owner") != "" || r.header.Get("X-Amz-Request-Payer") != "" {
			t.Fatalf("the default sent %v", r.header)
		}
	}

	client, bucket := fakeS3(t)
	for _, cfg := range []fss3.Config{
		{ExpectedBucketOwner: "acme"},
		{RequestPayer: "bucket-owner"},
		{PrivateACL: fss3.CannedPublicRead},
		{PrivateACL: "nope"},
	} {
		cfg.Client, cfg.Bucket = client, bucket
		_, err := fss3.New(cfg)
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
}

// TestPrivateACL proves VisibilityACL gives private objects the
// configured canned ACL and public ones public-read, and that the default
// stays "private".
func TestPrivateACL(t *testing.T) {
	ctx := t.Context()
	for _, tc := range []struct {
		acl  fss3.CannedACL
		want string
	}{{fss3.CannedBucketOwnerFullControl, "bucket-owner-full-control"}, {"", "private"}} {
		d, p := newDisk(t, fss3.Config{Visibility: fss3.VisibilityACL, PrivateACL: tc.acl})
		for v, want := range map[filesystem.Visibility]string{filesystem.VisibilityPrivate: tc.want, filesystem.VisibilityPublic: "public-read"} {
			if _, err := d.WriteBytes(ctx, string(v), []byte("x"), filesystem.WriteOptions{Visibility: v}); err != nil {
				t.Fatal(err)
			}
			puts := sentFor(p, http.MethodPut, "x-id=PutObject", string(v))
			if len(puts) == 0 || puts[len(puts)-1].header.Get("X-Amz-Acl") != want {
				t.Errorf("PrivateACL %q, %s write sent ACL %v, want %s", tc.acl, v, puts, want)
			}
		}
	}
}

// TestClock proves Config.Clock dates object-lock retentions, signs
// presigned URLs and forms, and sets their ExpiresAt.
func TestClock(t *testing.T) {
	ctx := t.Context()
	at := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	clock := func() time.Time { return at }
	d, p := lockedDisk(t, fss3.Config{ObjectLockDays: 30, Clock: clock})
	if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	puts := p.sent(http.MethodPut, "x-id=PutObject")
	if got := puts[len(puts)-1].header.Get("X-Amz-Object-Lock-Retain-Until-Date"); !strings.HasPrefix(got, at.AddDate(0, 0, 30).Format("2006-01-02")) {
		t.Errorf("retain until %q, want 30 days after the clock", got)
	}

	plain, _ := newDisk(t, fss3.Config{Clock: clock})
	req, err := plain.SignedURL(ctx, "k", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(req.URL)
	if got := u.Query().Get("X-Amz-Date"); got != at.Format("20060102T150405Z") {
		t.Errorf("X-Amz-Date = %s, want the clock's %s", got, at.Format("20060102T150405Z"))
	}
	if !req.ExpiresAt.Equal(at.Add(time.Minute)) {
		t.Errorf("ExpiresAt = %v, want %v", req.ExpiresAt, at.Add(time.Minute))
	}
	post, err := plain.PresignPost(ctx, filesystem.PostPolicy{KeyPrefix: "uploads", Expires: time.Minute, MaxSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := post.Fields["X-Amz-Date"]; got != at.Format("20060102T150405Z") {
		t.Errorf("POST X-Amz-Date = %s, want the clock's", got)
	}
	if !post.ExpiresAt.Equal(at.Add(time.Minute)) {
		t.Errorf("POST ExpiresAt = %v", post.ExpiresAt)
	}
}

// TestRoleSessionOptions proves the session policy, managed policies,
// tags (sorted), transitive keys and source identity reach STS, and that
// none is sent by default.
func TestRoleSessionOptions(t *testing.T) {
	ctx := t.Context()
	f := &fakeAssumer{life: time.Hour}
	o := roleOpts()
	o.Policy = `{"Version":"2012-10-17","Statement":[]}`
	o.PolicyARNs = []string{"arn:aws:iam::aws:policy/ReadOnlyAccess"}
	o.Tags = map[string]string{"tenant": "acme", "env": "prod"}
	o.TransitiveTagKeys = []string{"tenant"}
	o.SourceIdentity = "app-worker"
	creds, err := fss3.RoleCredentials(f, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := creds.Retrieve(ctx); err != nil {
		t.Fatal(err)
	}
	in := f.calls[0]
	if *in.Policy != o.Policy || *in.PolicyArns[0].Arn != o.PolicyARNs[0] || *in.SourceIdentity != "app-worker" ||
		len(in.Tags) != 2 || *in.Tags[0].Key != "env" || *in.Tags[1].Value != "acme" || in.TransitiveTagKeys[0] != "tenant" {
		t.Fatalf("AssumeRole input = %+v", in)
	}

	f = &fakeAssumer{life: time.Hour}
	creds, _ = fss3.RoleCredentials(f, roleOpts())
	if _, err := creds.Retrieve(ctx); err != nil {
		t.Fatal(err)
	}
	if in := f.calls[0]; in.Policy != nil || in.PolicyArns != nil || in.Tags != nil || in.SourceIdentity != nil {
		t.Fatalf("default AssumeRole input carries session options: %+v", in)
	}
}
