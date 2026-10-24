package gcs_test

import (
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gcs"
)

// lockNow is the fixed clock of the object-lock tests.
var lockNow = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

// takeUploads returns the recorded upload bodies joined, and forgets them.
func takeUploads(p *proxy) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := strings.Join(p.uploads, "\n")
	p.uploads = nil
	return b
}

// takeBodies returns the recorded PATCH and rewrite bodies joined, and
// forgets them.
func takeBodies(p *proxy) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := strings.Join(p.bodies, "\n")
	p.bodies = nil
	return b
}

// TestRequestOptionsObjectLock proves RetainUntil, TemporaryHold and
// CustomTime are sent with the write and the copy that create the object
// (no window before a later SetHold), only for the bound key, and that a
// write without them sends none (default unchanged).
func TestRequestOptionsObjectLock(t *testing.T) {
	ctx := t.Context()
	client, fake, p, _ := proxied(t)
	d := disk(t, gcs.Config{Client: client, Bucket: newBucket(t, fake), Clock: func() time.Time { return lockNow }})
	until := lockNow.Add(30 * 24 * time.Hour)
	due := lockNow.Add(14 * 24 * time.Hour)
	o := gcs.RequestOptions{RetainUntil: until, TemporaryHold: true, CustomTime: due}
	want := []string{`"temporaryHold":true`, `"customTime":"` + due.Format(time.RFC3339), `"mode":"Unlocked"`, `"retainUntilTime":"` + until.Format(time.RFC3339)}
	wctx, err := gcs.WithRequestOptions(ctx, d, "inv/1.pdf", o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(wctx, "inv/1.pdf", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	body := takeUploads(p)
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("write did not send %s: %s", w, body)
		}
	}
	// The same context on another key sends none of them.
	if _, err := d.WriteBytes(wctx, "inv/2.pdf", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if body := takeUploads(p); strings.Contains(body, "temporaryHold") || strings.Contains(body, "customTime") || strings.Contains(body, "retention") {
		t.Errorf("a write of another key inherited the options: %s", body)
	}
	cctx, err := gcs.WithRequestOptions(ctx, d, "inv/3.pdf", o)
	if err != nil {
		t.Fatal(err)
	}
	takeBodies(p)
	if _, err := d.Copy(cctx, "inv/2.pdf", "inv/3.pdf"); err != nil {
		t.Fatal(err)
	}
	body = takeBodies(p)
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("copy did not send %s: %s", w, body)
		}
	}
}

// TestRetainUntilOnlyLengthens proves a call's RetainUntil cannot shorten
// Config.RetainFor: the later date wins either way.
func TestRetainUntilOnlyLengthens(t *testing.T) {
	ctx := t.Context()
	client, fake, p, _ := proxied(t)
	d := disk(t, gcs.Config{Client: client, Bucket: newBucket(t, fake), RetainFor: 48 * time.Hour, Clock: func() time.Time { return lockNow }})
	for _, c := range []struct {
		name       string
		ask, wants time.Time
	}{
		{"shorter", lockNow.Add(time.Hour), lockNow.Add(48 * time.Hour)},
		{"longer", lockNow.Add(72 * time.Hour), lockNow.Add(72 * time.Hour)},
	} {
		rctx, err := gcs.WithRequestOptions(ctx, d, c.name, gcs.RequestOptions{RetainUntil: c.ask})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.WriteBytes(rctx, c.name, []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		if body := takeUploads(p); !strings.Contains(body, `"retainUntilTime":"`+c.wants.Format(time.RFC3339)) {
			t.Errorf("%s: retention sent %s, want %s", c.name, body, c.wants.Format(time.RFC3339))
		}
	}
}

// TestObjectLockRefusals proves a RetainUntil not in the future fails
// closed before anything is written, and that a presigned PUT or form,
// which cannot pin a retention, hold or Custom-Time, refuses them rather
// than hand out an upload that lands unprotected.
func TestObjectLockRefusals(t *testing.T) {
	ctx := t.Context()
	client, fake, _, _ := proxied(t)
	d := disk(t, gcs.Config{Client: client, Bucket: newBucket(t, fake), Clock: func() time.Time { return lockNow },
		Signing: &storage.SignedURLOptions{GoogleAccessID: signer, PrivateKey: pemKey(t)}})
	past, err := gcs.WithRequestOptions(ctx, d, "a.txt", gcs.RequestOptions{RetainUntil: lockNow})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.WriteBytes(past, "a.txt", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if _, err := d.Stat(ctx, "a.txt"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("a refused write left an object: %v", err)
	}
	for name, o := range map[string]gcs.RequestOptions{
		"retention":   {RetainUntil: lockNow.Add(time.Hour)},
		"hold":        {TemporaryHold: true},
		"custom time": {CustomTime: lockNow},
	} {
		rctx, err := gcs.WithRequestOptions(ctx, d, "a.txt", o)
		if err != nil {
			t.Fatal(err)
		}
		_, err = d.SignedURL(rctx, "a.txt", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
		if filesystem.CodeOf(err) != filesystem.CodeUnsupported {
			t.Errorf("%s: presigned PUT = %v, want CodeUnsupported", name, err)
		}
		_, err = d.PresignPost(rctx, filesystem.PostPolicy{Key: "a.txt", MaxSize: 1, Expires: time.Minute})
		if filesystem.CodeOf(err) != filesystem.CodeUnsupported {
			t.Errorf("%s: form = %v, want CodeUnsupported", name, err)
		}
		// A GET needs no upload settings, so it still signs.
		if _, err := d.SignedURL(rctx, "a.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute}); err != nil {
			t.Errorf("%s: presigned GET = %v", name, err)
		}
	}
}

// TestPrivateACL proves Config.PrivateACL is the ACL of a private write, a
// private presigned PUT or form (and of one asking no visibility), and the
// base ACL of a copy, that public objects keep publicRead, that the
// default is unchanged, and that a public, unknown or uniform-access
// PrivateACL is refused.
func TestPrivateACL(t *testing.T) {
	ctx := t.Context()
	client, fake, p, _ := proxied(t)
	signing := &storage.SignedURLOptions{GoogleAccessID: signer, PrivateKey: pemKey(t)}
	d := disk(t, gcs.Config{Client: client, Bucket: newBucket(t, fake), Visibility: gcs.VisibilityACL, PrivateACL: gcs.ACLBucketOwnerFullControl, Signing: signing})
	if _, err := d.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPrivate}); err != nil {
		t.Fatal(err)
	}
	if len(p.sent("POST", "uploadType=", "predefinedAcl=bucketOwnerFullControl")) != 1 {
		t.Errorf("a private write did not ask for bucketOwnerFullControl: %v", p.sent("POST", "uploadType="))
	}
	if _, err := d.WriteBytes(ctx, "b.txt", []byte("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic}); err != nil {
		t.Fatal(err)
	}
	if len(p.sent("POST", "uploadType=", "predefinedAcl=publicRead")) != 1 {
		t.Error("a public write did not keep publicRead")
	}
	if _, err := d.Copy(ctx, "a.txt", "c.txt"); err != nil {
		t.Fatal(err)
	}
	if len(p.sent("POST", "rewriteTo", "destinationPredefinedAcl=bucketOwnerFullControl")) != 1 {
		t.Errorf("a copy did not ask for bucketOwnerFullControl: %v", p.sent("POST", "rewriteTo"))
	}
	for _, c := range []struct {
		ask  filesystem.Visibility
		want string
	}{{"", "bucket-owner-full-control"}, {filesystem.VisibilityPrivate, "bucket-owner-full-control"}, {filesystem.VisibilityPublic, "public-read"}} {
		put, err := d.SignedURL(ctx, "a.txt", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, Visibility: c.ask})
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(put.URL)
		if put.Header.Get("X-Goog-Acl") != c.want || !strings.Contains(u.Query().Get("X-Goog-SignedHeaders"), "x-goog-acl") {
			t.Errorf("PUT %q: acl %q, want %q signed", c.ask, put.Header.Get("X-Goog-Acl"), c.want)
		}
		post, err := d.PresignPost(ctx, filesystem.PostPolicy{Key: "a.txt", MaxSize: 1, Expires: time.Minute, Visibility: c.ask})
		if err != nil {
			t.Fatal(err)
		}
		if post.Fields["acl"] != c.want {
			t.Errorf("POST %q: acl %q, want %q", c.ask, post.Fields["acl"], c.want)
		}
	}
	// Default: private writes are "private" and copies name no ACL.
	def := disk(t, gcs.Config{Client: client, Bucket: newBucket(t, fake), Visibility: gcs.VisibilityACL})
	if _, err := def.WriteBytes(ctx, "d.txt", []byte("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPrivate}); err != nil {
		t.Fatal(err)
	}
	if len(p.sent("POST", "uploadType=", "predefinedAcl=private")) != 1 {
		t.Error("the default private ACL changed")
	}
	if _, err := def.Copy(ctx, "d.txt", "e.txt"); err != nil {
		t.Fatal(err)
	}
	if got := p.sent("POST", "rewriteTo", "destinationPredefinedAcl"); len(got) != 1 {
		t.Errorf("a default copy named an ACL: %v", got)
	}
	for _, cfg := range []gcs.Config{
		{Visibility: gcs.VisibilityACL, PrivateACL: "publicRead"},
		{Visibility: gcs.VisibilityACL, PrivateACL: "authenticatedRead"},
		{Visibility: gcs.VisibilityACL, PrivateACL: "nope"},
		{PrivateACL: gcs.ACLBucketOwnerFullControl},
	} {
		cfg.Client, cfg.Bucket = client, "b"
		if _, err := gcs.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("New(%q, %q) = %v, want CodeInvalidArgument", cfg.Visibility, cfg.PrivateACL, err)
		}
	}
	if !slices.Contains(gcs.PrivateACLValues, gcs.ACLPrivate) {
		t.Error("PrivateACLValues lacks the default")
	}
}
