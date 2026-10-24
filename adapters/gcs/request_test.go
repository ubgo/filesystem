package gcs_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gcs"
)

// tenantKMSKey is a key RequestOptions chooses over the disk's.
const tenantKMSKey = "projects/p/locations/eu/keyRings/r/cryptoKeys/tenant-7"

// TestRequestOptions proves the call's KMS key and storage class override
// the disk's on a write, a copy and a presigned PUT and form for exactly
// the bound key, and that any other key keeps the disk's (default
// unchanged).
func TestRequestOptions(t *testing.T) {
	ctx := t.Context()
	client, fake, p, _ := proxied(t)
	d := disk(t, gcs.Config{Client: client, Bucket: newBucket(t, fake), KMSKeyName: testKMSKey, Signing: &storage.SignedURLOptions{GoogleAccessID: signer, PrivateKey: pemKey(t)}})
	rctx, err := gcs.WithRequestOptions(ctx, d, "t/a.txt", gcs.RequestOptions{KMSKeyName: tenantKMSKey, StorageClass: gcs.ClassNearline})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(rctx, "t/a.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(p.sent("POST", "uploadType=", "kmsKeyName="+url.QueryEscape(tenantKMSKey))) == 0 {
		t.Error("write did not name the call's KMS key")
	}
	if e, err := d.Stat(ctx, "t/a.txt"); err != nil || e.StorageClass != string(gcs.ClassNearline) {
		t.Errorf("class after write = %q (%v), want NEARLINE", e.StorageClass, err)
	}
	// Another key with the same context keeps the disk's key.
	if _, err := d.WriteBytes(rctx, "t/b.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(p.sent("POST", "uploadType=", "kmsKeyName="+url.QueryEscape(testKMSKey))) == 0 {
		t.Error("a write of another key did not keep the disk's KMS key")
	}
	cctx, err := gcs.WithRequestOptions(ctx, d, "t/c.txt", gcs.RequestOptions{KMSKeyName: tenantKMSKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Copy(cctx, "t/b.txt", "t/c.txt"); err != nil {
		t.Fatal(err)
	}
	if len(p.sent("POST", "rewriteTo", "destinationKmsKeyName="+url.QueryEscape(tenantKMSKey))) == 0 {
		t.Error("copy did not name the call's KMS key")
	}
	// A class the call asks for is the copy's class.
	dctx, err := gcs.WithRequestOptions(ctx, d, "t/d.txt", gcs.RequestOptions{StorageClass: gcs.ClassColdline})
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.bodies = nil
	p.mu.Unlock()
	if _, err := d.Copy(dctx, "t/b.txt", "t/d.txt"); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	body := strings.Join(p.bodies, "")
	p.mu.Unlock()
	if !strings.Contains(body, `"storageClass":"COLDLINE"`) {
		t.Errorf("copy did not ask for the call's class: %s", body)
	}
	put, err := d.SignedURL(rctx, "t/a.txt", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(put.URL)
	signed := u.Query().Get("X-Goog-SignedHeaders")
	if !strings.Contains(signed, "x-goog-encryption-kms-key-name") || put.Header.Get("X-Goog-Encryption-Kms-Key-Name") != tenantKMSKey ||
		!strings.Contains(signed, "x-goog-storage-class") || put.Header.Get("X-Goog-Storage-Class") != string(gcs.ClassNearline) {
		t.Errorf("presigned PUT = %s %v", put.URL, put.Header)
	}
	post, err := d.PresignPost(rctx, filesystem.PostPolicy{Key: "t/a.txt", MaxSize: 1, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if post.Fields["X-Goog-Encryption-Kms-Key-Name"] != tenantKMSKey || post.Fields["x-goog-storage-class"] != string(gcs.ClassNearline) {
		t.Errorf("form fields = %v", post.Fields)
	}
	// Without options a presigned PUT signs no class, as before.
	put, err = d.SignedURL(ctx, "t/a.txt", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	if err != nil || put.Header.Get("X-Goog-Storage-Class") != "" || put.Header.Get("X-Goog-Encryption-Kms-Key-Name") != testKMSKey {
		t.Errorf("default presigned PUT = %v %v", put.Header, err)
	}
}

// TestRequestOptionsRefusals proves a bad call option fails closed before
// anything is written or signed.
func TestRequestOptionsRefusals(t *testing.T) {
	ctx := t.Context()
	client, fake, _, _ := proxied(t)
	d := disk(t, gcs.Config{Client: client, Bucket: newBucket(t, fake), Signing: &storage.SignedURLOptions{GoogleAccessID: signer, PrivateKey: pemKey(t)}})
	bad, err := gcs.WithRequestOptions(ctx, d, "a.txt", gcs.RequestOptions{StorageClass: "WARM"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.WriteBytes(bad, "a.txt", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if _, err := d.Stat(ctx, "a.txt"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("a refused write left an object: %v", err)
	}
	_, err = d.SignedURL(bad, "a.txt", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	// Every other operation that honours the options refuses them too,
	// before it calls GCS.
	_, err = d.Copy(bad, "src.txt", "a.txt")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = d.PresignPost(bad, filesystem.PostPolicy{Key: "a.txt", MaxSize: 1, Expires: time.Minute})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	versioned := disk(t, gcs.Config{Client: client, Bucket: newBucket(t, fake), Versioned: true})
	vbad, err := gcs.WithRequestOptions(ctx, versioned, "a.txt", gcs.RequestOptions{StorageClass: "WARM"})
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, versioned.SetStorageClass(vbad, "a.txt", string(gcs.ClassColdline)), filesystem.CodeInvalidArgument)
	wantCode(t, versioned.RestoreVersion(vbad, "a.txt", "1"), filesystem.CodeInvalidArgument)
	_, err = gcs.WithRequestOptions(ctx, d, "../x", gcs.RequestOptions{})
	if err == nil {
		t.Error("an invalid key was bound")
	}
	ck, err := gcs.NewCustomerKey(make([]byte, gcs.CustomerKeySize))
	if err != nil {
		t.Fatal(err)
	}
	csek := disk(t, gcs.Config{Client: client, Bucket: newBucket(t, fake), CustomerKey: ck})
	kctx, err := gcs.WithRequestOptions(ctx, csek, "a.txt", gcs.RequestOptions{KMSKeyName: tenantKMSKey})
	if err != nil {
		t.Fatal(err)
	}
	_, err = csek.WriteBytes(kctx, "a.txt", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeInvalidArgument)
}

// TestPresignVisibility proves an ACL-mode disk signs a visibility asked
// for as x-goog-acl on a PUT and pins it as the acl field of a form, signs
// nothing when none is asked (as before), and that a uniform-access disk
// refuses rather than issue an upload GCS would reject.
func TestPresignVisibility(t *testing.T) {
	ctx := t.Context()
	srv := fakeServer(t)
	d := disk(t, gcs.Config{Client: srv.Client(), Bucket: "media", Visibility: gcs.VisibilityACL, Signing: &storage.SignedURLOptions{GoogleAccessID: signer, PrivateKey: pemKey(t)}})
	if !d.Capabilities().PresignVisibility {
		t.Fatal("an ACL-mode disk does not declare PresignVisibility")
	}
	for _, c := range []struct {
		ask  filesystem.Visibility
		want string
	}{{"", ""}, {filesystem.VisibilityPublic, "public-read"}, {filesystem.VisibilityPrivate, "private"}} {
		put, err := d.SignedURL(ctx, "a.png", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, Visibility: c.ask})
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(put.URL)
		signed := strings.Contains(u.Query().Get("X-Goog-SignedHeaders"), "x-goog-acl")
		if put.Header.Get("X-Goog-Acl") != c.want || signed != (c.want != "") {
			t.Errorf("PUT %q: header %q signed %t, want %q", c.ask, put.Header.Get("X-Goog-Acl"), signed, c.want)
		}
		post, err := d.PresignPost(ctx, filesystem.PostPolicy{Key: "a.png", MaxSize: 1, Expires: time.Minute, Visibility: c.ask})
		if err != nil {
			t.Fatal(err)
		}
		if post.Fields["acl"] != c.want {
			t.Errorf("POST %q: acl field %q, want %q", c.ask, post.Fields["acl"], c.want)
		}
	}
	uniform := disk(t, gcs.Config{Client: srv.Client(), Bucket: "media", Signing: &storage.SignedURLOptions{GoogleAccessID: signer, PrivateKey: pemKey(t)}})
	if uniform.Capabilities().PresignVisibility {
		t.Fatal("a uniform-access disk declares PresignVisibility")
	}
	_, err := uniform.SignedURL(ctx, "a.png", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, Visibility: filesystem.VisibilityPublic})
	wantCode(t, err, filesystem.CodeUnsupported)
	drv, err := gcs.New(gcs.Config{Client: srv.Client(), Bucket: "media", Signing: &storage.SignedURLOptions{GoogleAccessID: signer, PrivateKey: pemKey(t)}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = drv.PresignPost(ctx, filesystem.PostPolicy{Key: "a.png", MaxSize: 1, Expires: time.Minute, Visibility: filesystem.VisibilityPublic})
	wantCode(t, err, filesystem.CodeUnsupported)
	// The driver itself refuses too, for a caller that skips the Disk.
	_, err = drv.Presign(ctx, "a.png", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, Visibility: filesystem.VisibilityPublic})
	wantCode(t, err, filesystem.CodeUnsupported)
}
