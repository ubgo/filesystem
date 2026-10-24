package s3_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// Per-tenant KMS keys of the tests below.
const (
	tenantAKMS = "arn:aws:kms:eu-west-1:111122223333:key/aaaaaaaa-0000-0000-0000-000000000001"
	tenantBKMS = "arn:aws:kms:eu-west-1:111122223333:key/bbbbbbbb-0000-0000-0000-000000000002"
	callKMS    = "arn:aws:kms:eu-west-1:111122223333:alias/one-off"
)

// tenantKMS is a Config.SSEKMSKeyFor that picks a key by tenant prefix.
func tenantKMS(key string) string {
	switch {
	case strings.HasPrefix(key, "tenants/a/"):
		return tenantAKMS
	case strings.HasPrefix(key, "tenants/b/"):
		return tenantBKMS
	}
	return ""
}

// sentFor returns the requests of method whose query contains query and
// whose path contains path.
func sentFor(p *s3Proxy, method, query, path string) []recorded {
	var out []recorded
	for _, r := range p.sent(method, query) {
		if strings.Contains(r.path, path) {
			out = append(out, r)
		}
	}
	return out
}

// TestSSEKMSKeyPerTenant proves one disk with Config.SSEKMSKeyFor sends
// each tenant's KMS key on every write path of that tenant's scoped disk
// (the resolver sees the key under the scope's prefix), no key outside any
// tenant, and that RequestOptions.SSEKMSKeyID overrides it for one key.
func TestSSEKMSKeyPerTenant(t *testing.T) {
	ctx := t.Context()
	d, p := newDisk(t, fss3.Config{SSEKMSKeyFor: tenantKMS})
	a, err := d.Scope("a", "tenants/a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.Scope("b", "tenants/b")
	if err != nil {
		t.Fatal(err)
	}
	expect := func(what string, got []recorded, sse, key string) {
		t.Helper()
		if len(got) == 0 {
			t.Fatalf("%s: no request recorded", what)
		}
		for _, r := range got {
			if r.sse != sse || r.kmsKey != key {
				t.Errorf("%s %s %s sent sse %q key %q, want %q %q", what, r.method, r.path, r.sse, r.kmsKey, sse, key)
			}
		}
	}
	kms := string(types.ServerSideEncryptionAwsKms)
	for _, c := range []struct {
		disk *filesystem.Disk
		path string
		key  string
	}{{a, "tenants/a/", tenantAKMS}, {b, "tenants/b/", tenantBKMS}} {
		if _, err := c.disk.WriteBytes(ctx, "f.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		expect("write "+c.path, sentFor(p, http.MethodPut, "x-id=PutObject", c.path+"f.txt"), kms, c.key)
		if _, err := c.disk.Copy(ctx, "f.txt", "g.txt"); err != nil {
			t.Fatal(err)
		}
		expect("copy "+c.path, sentFor(p, http.MethodPut, "x-id=CopyObject", c.path+"g.txt"), kms, c.key)
		if _, err := c.disk.CreateMultipart(ctx, "m.bin", filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		expect("multipart "+c.path, sentFor(p, http.MethodPost, "uploads", c.path+"m.bin"), kms, c.key)
		put, err := c.disk.SignedURL(ctx, "direct.bin", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if put.Header.Get("X-Amz-Server-Side-Encryption") != kms || put.Header.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id") != c.key {
			t.Errorf("presigned PUT %s headers = %v", c.path, put.Header)
		}
		post, err := c.disk.PresignPost(ctx, filesystem.PostPolicy{KeyPrefix: "up", MaxSize: 1 << 10, Expires: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if post.Fields["x-amz-server-side-encryption"] != kms || post.Fields["x-amz-server-side-encryption-aws-kms-key-id"] != c.key {
			t.Errorf("presigned POST %s fields = %v", c.path, post.Fields)
		}
	}
	if _, err := d.WriteBytes(ctx, "shared.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	expect("write outside any tenant", sentFor(p, http.MethodPut, "x-id=PutObject", "/shared.txt"), "", "")
	octx, err := fss3.WithRequestOptions(ctx, a, "one-off.txt", fss3.RequestOptions{SSEKMSKeyID: callKMS})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.WriteBytes(octx, "one-off.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	expect("write with a call key", sentFor(p, http.MethodPut, "x-id=PutObject", "tenants/a/one-off.txt"), kms, callKMS)
	if _, err := a.WriteBytes(octx, "other.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	expect("another key under a call key's context", sentFor(p, http.MethodPut, "x-id=PutObject", "tenants/a/other.txt"), kms, tenantAKMS)
}

// TestSSEKMSKeyFallbacks covers the order and the modes: a resolver that
// returns "" falls back to Config.SSEKMSKeyID, a DSSE disk keeps DSSE for
// a resolved key, and a call key on a disk without encryption selects
// aws:kms for that write only.
func TestSSEKMSKeyFallbacks(t *testing.T) {
	ctx := t.Context()
	d, p := newDisk(t, fss3.Config{SSEKMSKeyFor: tenantKMS, SSEKMSKeyID: testKMSKey, ServerSideEncryption: types.ServerSideEncryptionAwsKmsDsse})
	for _, key := range []string{"tenants/a/x", "other/x"} {
		if _, err := d.WriteBytes(ctx, key, []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	dsse := string(types.ServerSideEncryptionAwsKmsDsse)
	for path, want := range map[string]string{"tenants/a/x": tenantAKMS, "other/x": testKMSKey} {
		got := sentFor(p, http.MethodPut, "x-id=PutObject", path)
		if len(got) == 0 {
			t.Fatalf("%s: no request", path)
		}
		for _, r := range got {
			if r.sse != dsse || r.kmsKey != want {
				t.Errorf("%s sent sse %q key %q, want %q %q", path, r.sse, r.kmsKey, dsse, want)
			}
		}
	}
	plain, pp := newDisk(t, fss3.Config{})
	octx, err := fss3.WithRequestOptions(ctx, plain, "k", fss3.RequestOptions{SSEKMSKeyID: callKMS})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.WriteBytes(octx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := plain.WriteBytes(ctx, "k2", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][2]string{"/k": {string(types.ServerSideEncryptionAwsKms), callKMS}, "/k2": {"", ""}} {
		got := sentFor(pp, http.MethodPut, "x-id=PutObject", path)
		if len(got) == 0 {
			t.Fatalf("%s: no request", path)
		}
		for _, r := range got {
			if !strings.HasSuffix(r.path, path) {
				continue
			}
			if r.sse != want[0] || r.kmsKey != want[1] {
				t.Errorf("%s sent sse %q key %q, want %v", path, r.sse, r.kmsKey, want)
			}
		}
	}
}

// TestSSEKMSKeyRefusals: a KMS key cannot reach an SSE-S3 or SSE-C disk,
// whether from Config.SSEKMSKeyFor (refused by New) or from one call's
// RequestOptions (refused before any byte is sent).
func TestSSEKMSKeyRefusals(t *testing.T) {
	ctx := t.Context()
	client, bucket, _ := fakeS3WithProxy(t)
	ck, err := fss3.NewCustomerKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]fss3.Config{
		"resolver with SSE-S3": {ServerSideEncryption: types.ServerSideEncryptionAes256, SSEKMSKeyFor: tenantKMS},
		"resolver with SSE-C":  {SSECustomerKey: ck, SSEKMSKeyFor: tenantKMS},
	} {
		cfg.Client, cfg.Bucket = client, bucket
		if _, err := fss3.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: New = %v", name, err)
		}
	}
	for name, cfg := range map[string]fss3.Config{
		"SSE-S3": {ServerSideEncryption: types.ServerSideEncryptionAes256},
		"SSE-C":  {SSECustomerKey: ck},
	} {
		d, p := newDisk(t, cfg)
		octx, err := fss3.WithRequestOptions(ctx, d, "k", fss3.RequestOptions{SSEKMSKeyID: callKMS})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.WriteBytes(octx, "k", []byte("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s disk, call KMS key: write = %v", name, err)
		}
		if got := p.sent(http.MethodPut, "x-id=PutObject"); len(got) != 0 {
			t.Errorf("%s disk: refused write still sent %d requests", name, len(got))
		}
	}
}
