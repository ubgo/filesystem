package s3_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
	"github.com/ubgo/filesystem/filesystemtest"
)

// EnvTestDSN names the environment variable holding a DSN of a real
// S3-compatible bucket, e.g.
//
//	s3://filesystem-test?region=us-east-1&endpoint=http://127.0.0.1:8333&path_style=true
//
// When it is set, the conformance suite runs against that store, each case
// under its own random prefix (cleaned up afterwards). Credentials come from
// the usual AWS environment variables. `task test:s3` starts a local
// SeaweedFS and sets it.
const EnvTestDSN = "FILESYSTEM_S3_TEST_DSN"

func TestConformanceAgainstARealStore(t *testing.T) {
	dsn := os.Getenv(EnvTestDSN)
	if dsn == "" {
		t.Skipf("set %s to run the suite against a real S3-compatible store", EnvTestDSN)
	}
	mux := filesystem.NewMux()
	if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
		t.Fatal(err)
	}
	ensureBucket(t, mux, dsn)
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		prefix := "conformance-" + strings.ToLower(rand.Text())
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		d, err := mux.Open(t.Context(), "real", dsn+sep+filesystem.ParamPrefix+"="+prefix)
		if err != nil {
			t.Fatal(err)
		}
		root, err := mux.Open(t.Context(), "root", dsn)
		if err != nil {
			t.Fatal(err)
		}
		// t.Context is already cancelled when cleanups run.
		t.Cleanup(func() { _ = root.DeletePrefix(context.Background(), prefix) })
		return d
	})
}

// ensureBucket creates the DSN's bucket on a fresh local store.
func ensureBucket(t *testing.T, mux *filesystem.Mux, dsn string) {
	t.Helper()
	d, err := mux.Open(t.Context(), "setup", dsn)
	if err != nil {
		t.Fatal(err)
	}
	client, _ := filesystem.As[*s3.Client](d)
	bucket := strings.TrimPrefix(strings.SplitN(dsn, "?", 2)[0], fss3.Scheme+"://")
	_, err = client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if err != nil && !errors.As(err, &owned) && !errors.As(err, &exists) {
		t.Fatalf("create bucket %s: %v", bucket, err)
	}
}

// TestPresignHeadersAgainstARealStore proves, on a store that verifies
// SigV4, that a presigned PUT pins its stored headers: an upload without
// them is refused, and one with them stores them. A store that does not
// verify signatures (SeaweedFS without -s3.config) accepts both, which is
// why the capability can be turned off per DSN (presign_headers=false).
func TestPresignHeadersAgainstARealStore(t *testing.T) {
	dsn := os.Getenv(EnvTestDSN)
	if dsn == "" {
		t.Skipf("set %s to run against a real S3-compatible store", EnvTestDSN)
	}
	ctx := t.Context()
	mux := filesystem.NewMux()
	if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
		t.Fatal(err)
	}
	ensureBucket(t, mux, dsn)
	d, err := mux.Open(ctx, "real", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Capabilities().PresignHeaders {
		t.Skip("the DSN turns presign_headers off")
	}
	key := "presign-headers-" + strings.ToLower(rand.Text()) + ".txt"
	t.Cleanup(func() { _ = d.Delete(context.Background(), key) })
	h := filesystem.HTTPHeaders{CacheControl: "public, max-age=60", ContentDisposition: `attachment; filename="r.txt"`}
	put, err := d.SignedURL(ctx, key, filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, ContentType: "text/plain", Headers: h})
	if err != nil {
		t.Fatal(err)
	}
	without := put.Header.Clone()
	without.Del(filesystem.HeaderCacheControl)
	if status, body := get(t, http.MethodPut, put.URL, strings.NewReader("x"), without); status < 400 {
		t.Fatalf("PUT without a signed header = %d %s, want refused", status, body)
	}
	if status, body := get(t, http.MethodPut, put.URL, strings.NewReader("x"), put.Header); status != http.StatusOK {
		t.Fatalf("PUT with the signed headers = %d %s", status, body)
	}
	e, err := d.Stat(ctx, key)
	if err != nil || e.Headers.CacheControl != h.CacheControl || e.Headers.ContentDisposition != h.ContentDisposition {
		t.Fatalf("stored headers = %+v, %v", e.Headers, err)
	}
}

// TestSweepUploadsAgainstARealStore proves the opt-in sweep on a real
// store: an upload in progress survives a cutoff before it started and is
// aborted by one after. It opens the bucket without a prefix, because a
// prefixed (scoped) disk never forwards SweepAbandoned, so the conformance
// suite's sweep case cannot run there.
func TestSweepUploadsAgainstARealStore(t *testing.T) {
	dsn := os.Getenv(EnvTestDSN)
	if dsn == "" {
		t.Skipf("set %s to run against a real S3-compatible store", EnvTestDSN)
	}
	ctx := t.Context()
	mux := filesystem.NewMux()
	if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
		t.Fatal(err)
	}
	ensureBucket(t, mux, dsn)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	d, err := mux.Open(ctx, "real", dsn+sep+fss3.ParamSweepUploads+"=true")
	if err != nil {
		t.Fatal(err)
	}
	key := "sweep-" + strings.ToLower(rand.Text()) + ".bin"
	id, err := d.CreateMultipart(ctx, key, filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.AbortMultipart(context.Background(), key, id) })
	if _, err := d.SweepAbandoned(ctx, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ListParts(ctx, key, id); err != nil {
		t.Fatalf("an upload in progress was swept by an older cutoff: %v", err)
	}
	n, err := d.SweepAbandoned(ctx, time.Now().Add(time.Hour))
	if err != nil || n < 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	if _, err := d.ListParts(ctx, key, id); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("upload after the sweep = %v, want not found", err)
	}
}

// TestCORSAgainstARealStore sets the recommended direct-upload rule on a
// real store and checks a browser's preflight for a signed PUT is allowed
// for the configured origin and not for another.
func TestCORSAgainstARealStore(t *testing.T) {
	dsn := os.Getenv(EnvTestDSN)
	if dsn == "" {
		t.Skipf("set %s to run against a real S3-compatible store", EnvTestDSN)
	}
	ctx := t.Context()
	mux := filesystem.NewMux()
	if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
		t.Fatal(err)
	}
	ensureBucket(t, mux, dsn)
	d, err := mux.Open(ctx, "real", dsn)
	if err != nil {
		t.Fatal(err)
	}
	client, _ := filesystem.As[*s3.Client](d)
	bucket := strings.TrimPrefix(strings.SplitN(dsn, "?", 2)[0], fss3.Scheme+"://")
	const origin = "https://app.example.com"
	if err := fss3.SetCORS(ctx, client, bucket, fss3.DirectUploadCORS(origin)); err != nil {
		t.Fatal(err)
	}
	put, err := d.SignedURL(ctx, "cors-probe.txt", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, ContentType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	preflight := func(from string) string {
		req, _ := http.NewRequestWithContext(ctx, http.MethodOptions, put.URL, nil)
		req.Header.Set("Origin", from)
		req.Header.Set("Access-Control-Request-Method", http.MethodPut)
		req.Header.Set("Access-Control-Request-Headers", "content-type")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.Header.Get("Access-Control-Allow-Origin")
	}
	if got := preflight(origin); got != origin && got != "*" {
		t.Errorf("preflight from %s allowed %q", origin, got)
	}
	if got := preflight("https://evil.example.net"); got == "https://evil.example.net" || got == "*" {
		t.Errorf("preflight from another origin allowed %q", got)
	}
}

// TestPresignPostAgainstARealStore proves a real store enforces the POST
// policy: a form upload inside it is stored with its type and headers,
// and one too large, of another type, or under another key is refused.
func TestPresignPostAgainstARealStore(t *testing.T) {
	dsn := os.Getenv(EnvTestDSN)
	if dsn == "" {
		t.Skipf("set %s to run against a real S3-compatible store", EnvTestDSN)
	}
	ctx := t.Context()
	mux := filesystem.NewMux()
	if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
		t.Fatal(err)
	}
	ensureBucket(t, mux, dsn)
	d, err := mux.Open(ctx, "real", dsn)
	if err != nil {
		t.Fatal(err)
	}
	dir := "post-" + strings.ToLower(rand.Text())
	t.Cleanup(func() { _ = d.DeletePrefix(context.Background(), dir) })
	h := filesystem.HTTPHeaders{CacheControl: "no-store"}
	post, err := d.PresignPost(ctx, filesystem.PostPolicy{Key: dir + "/a.txt", ContentType: "text/plain", MinSize: 1, MaxSize: 10, Expires: time.Minute, Headers: h})
	if err != nil {
		t.Fatal(err)
	}
	with := func(k, v string) map[string]string {
		f := map[string]string{}
		for kk, vv := range post.Fields {
			f[kk] = vv
		}
		f[k] = v
		return f
	}
	for name, c := range map[string]struct {
		fields map[string]string
		body   string
	}{
		"too large":    {post.Fields, "this is longer than ten bytes"},
		"another type": {with("Content-Type", "text/html"), "hi"},
		"another key":  {with("key", dir+"/b.txt"), "hi"},
	} {
		if status, body := postForm(t, post.URL, c.fields, "a.txt", []byte(c.body)); status < 400 {
			t.Errorf("%s: form post = %d %s, want refused", name, status, body)
		}
	}
	if status, body := postForm(t, post.URL, post.Fields, "a.txt", []byte("hello")); status >= 300 {
		t.Fatalf("form post = %d %s", status, body)
	}
	short, err := d.PresignPost(ctx, filesystem.PostPolicy{Key: dir + "/late.txt", MaxSize: 10, Expires: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if status, body := postForm(t, short.URL, short.Fields, "late.txt", []byte("hi")); status < 400 {
		t.Errorf("expired form post = %d %s, want refused", status, body)
	}
	e, err := d.Stat(ctx, dir+"/a.txt")
	if err != nil || e.Size != 5 || e.ContentType != "text/plain" || e.Headers.CacheControl != "no-store" {
		t.Errorf("stored = %+v, %v", e, err)
	}
}

// TestChecksumAgainstARealStore proves the store itself refuses a body
// whose SHA-256 differs from the one sent (the driver is called directly,
// since the Disk would refuse first), and stores one that matches.
func TestChecksumAgainstARealStore(t *testing.T) {
	dsn := os.Getenv(EnvTestDSN)
	if dsn == "" {
		t.Skipf("set %s to run against a real S3-compatible store", EnvTestDSN)
	}
	ctx := t.Context()
	mux := filesystem.NewMux()
	if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
		t.Fatal(err)
	}
	ensureBucket(t, mux, dsn)
	d, err := mux.Open(ctx, "real", dsn)
	if err != nil {
		t.Fatal(err)
	}
	client, _ := filesystem.As[*s3.Client](d)
	bucket := strings.TrimPrefix(strings.SplitN(dsn, "?", 2)[0], fss3.Scheme+"://")
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	key := "checksum-" + strings.ToLower(rand.Text()) + ".txt"
	t.Cleanup(func() { _ = drv.Delete(context.Background(), key) })
	other := sha256.Sum256([]byte("other"))
	if _, err := drv.Write(ctx, key, strings.NewReader("hello"), filesystem.WriteOptions{SHA256: hex.EncodeToString(other[:]), SizeHint: 5}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("mismatching body = %v, want invalid_argument", err)
	}
	if _, err := drv.Stat(ctx, key); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("after the refused write: %v", err)
	}
	good := sha256.Sum256([]byte("hello"))
	if _, err := drv.Write(ctx, key, strings.NewReader("hello"), filesystem.WriteOptions{SHA256: hex.EncodeToString(good[:]), SizeHint: 5}); err != nil {
		t.Errorf("matching write = %v", err)
	}
}
