package share_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/share"
	"github.com/ubgo/filesystem/upload"
)

// listOnly is a collection that cannot receive files.
type listOnly struct{ kind string }

func (l listOnly) Kind() string { return l.kind }
func (listOnly) Files(context.Context, *records.Store, string, string, int) ([]records.File, error) {
	return nil, nil
}
func (listOnly) Contains(context.Context, *records.Store, string, string, string) (bool, error) {
	return false, nil
}

// TestOwnersRefusesBadRefs: the owner collection refuses a ref that is not
// an OwnerRef, or names an owner type the Store does not know, rather
// than reading an empty collection.
func TestOwnersRefusesBadRefs(t *testing.T) {
	ctx := t.Context()
	u := newSuite(t, 0)
	f := u.file(t, "a.txt")
	var o share.Owners
	for _, ref := range []string{"post/p1", "%zz/p1/inbox"} {
		if _, err := o.Files(ctx, u.store, "t", ref, 10); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("Files(%q) = %v", ref, err)
		}
		if _, err := o.Contains(ctx, u.store, "t", ref, f.ID); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("Contains(%q) = %v", ref, err)
		}
		if err := o.Accept(ctx, u.store, "t", ref, f); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("Accept(%q) = %v", ref, err)
		}
	}
	unknown := share.OwnerRef(records.Owner{Type: "user", ID: "u1"}, "inbox")
	if _, err := o.Files(ctx, u.store, "t", unknown, 10); err == nil {
		t.Fatal("Files of an unknown owner type")
	}
	if _, err := o.Contains(ctx, u.store, "t", unknown, f.ID); err == nil {
		t.Fatal("Contains of an unknown owner type")
	}
}

// TestRequestLinksNeedAReceiver: a request link needs an installed
// collection that can receive files; a plugin whose collection of the
// link's kind cannot receive answers its deposits as gone.
func TestRequestLinksNeedAReceiver(t *testing.T) {
	ctx := t.Context()
	u := newSuite(t, 0)
	if _, err := u.plugin.CreateRequest(ctx, u.store, "t", share.Target{Kind: "album", Ref: "x"}, share.Options{TTL: time.Hour}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("CreateRequest of an unknown kind = %v", err)
	}
	listing := share.New(share.WithCollections(listOnly{kind: "album"}))
	if _, err := listing.CreateRequest(ctx, u.store, "t", share.Target{Kind: "album", Ref: "x"}, share.Options{TTL: time.Hour}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("CreateRequest into a collection that cannot receive = %v", err)
	}
	post := records.Owner{Type: "post", ID: "p1"}
	sh, err := u.plugin.CreateRequest(ctx, u.store, "t", share.Target{Kind: share.KindOwner, Ref: share.OwnerRef(post, "inbox")}, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	other := share.New(share.WithCollections(listOnly{kind: share.KindOwner}))
	_, err = other.Deposit(ctx, u.store, sh.Token, "media", strings.NewReader("x"), "x.txt", anyText)
	gone(t, err)
}

// revoker revokes a link the first time it is read: a revocation landing
// while a deposit's body is still being received.
type revoker struct {
	body   io.Reader
	revoke func()
}

func (r *revoker) Read(p []byte) (int, error) {
	if r.revoke != nil {
		r.revoke()
		r.revoke = nil
	}
	return r.body.Read(p)
}

// TestDepositOnALinkRevokedMeanwhile: a request link revoked while a
// deposit's body arrives spends nothing and keeps no file: the deposit is
// gone and its upload rolled back.
func TestDepositOnALinkRevokedMeanwhile(t *testing.T) {
	ctx := t.Context()
	u := newSuite(t, 0)
	post := records.Owner{Type: "post", ID: "p1"}
	sh, err := u.plugin.CreateRequest(ctx, u.store, "t", share.Target{Kind: share.KindOwner, Ref: share.OwnerRef(post, "inbox")}, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	body := &revoker{body: strings.NewReader("late"), revoke: func() {
		if err := u.plugin.Revoke(ctx, u.store, "t", sh.ID); err != nil {
			t.Error(err)
		}
	}}
	_, err = u.plugin.Deposit(ctx, u.store, sh.Token, "media", body, "late.txt", anyText)
	gone(t, err)
	if page, _ := u.store.Files(ctx, records.FileQuery{Scope: "t"}); len(page.Files) != 0 {
		t.Fatalf("the refused deposit kept %d files", len(page.Files))
	}
}

// TestCollectionLinkRefusals: file and collection calls on a link of the
// other kind, an unknown token, a collection that went away, and a file
// trashed in a shared folder are all the uniform not-found.
func TestCollectionLinkRefusals(t *testing.T) {
	ctx := t.Context()
	u := newSuite(t, 0)
	f := u.file(t, "a.txt")
	dir, err := u.folders.Create(ctx, u.store, "t", folders.RootID, "docs")
	if err != nil {
		t.Fatal(err)
	}
	if err := u.folders.Put(ctx, u.store, "t", f.ID, dir.ID); err != nil {
		t.Fatal(err)
	}
	fileLink, err := u.plugin.Create(ctx, u.store, "t", f.ID, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	dirLink, err := u.plugin.CreateFor(ctx, u.store, "t", share.Target{Kind: folders.KindFolder, Ref: dir.ID}, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	_, err = u.plugin.ResolveFile(ctx, u.store, fileLink.Token, f.ID)
	gone(t, err)
	_, err = u.plugin.Resolve(ctx, u.store, dirLink.Token)
	gone(t, err)
	_, err = u.plugin.Check(ctx, u.store, dirLink.Token)
	gone(t, err)
	_, err = u.plugin.ResolveFile(ctx, u.store, "no-such-token", f.ID)
	gone(t, err)
	_, err = u.plugin.CheckFile(ctx, u.store, "no-such-token", f.ID)
	gone(t, err)

	if err := u.store.Trash(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	_, err = u.plugin.ResolveFile(ctx, u.store, dirLink.Token, f.ID)
	gone(t, err)
	if uses := u.uses(t, dirLink); uses != 0 {
		t.Fatalf("a refused download spent %d uses", uses)
	}
	if err := u.store.Restore(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if err := u.folders.Put(ctx, u.store, "t", f.ID, folders.RootID); err != nil {
		t.Fatal(err)
	}
	if err := u.folders.Delete(ctx, u.store, "t", dir.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err = u.plugin.List(ctx, u.store, dirLink.Token)
	gone(t, err)
}

// TestValidationOfPasswordsAndStats: a password longer than
// MaxPasswordBytes, a recent count outside [0, MaxRecentUses] and a
// viewer wider than its column are CodeInvalidArgument, and the refused
// download spends nothing.
func TestValidationOfPasswordsAndStats(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, true)
	if _, err := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour, Password: strings.Repeat("p", share.MaxPasswordBytes+1)}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a long password = %v", err)
	}
	sh, err := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{-1, share.MaxRecentUses + 1} {
		if _, err := e.plugin.Stats(ctx, e.store, "t", sh.ID, n); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("Stats(recent %d) = %v", n, err)
		}
	}
	wide := records.WithActor(ctx, strings.Repeat("v", records.MaxActorBytes+1))
	if _, err := e.plugin.Resolve(wide, e.store, sh.Token); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a download by a wide viewer = %v", err)
	}
	if n := e.downloads(t, sh.ID); n != 0 {
		t.Fatalf("the refused download spent %d", n)
	}
}

// TestHandlerEdges: a file path under a file link, a POST to a file link
// without a password, a collection link without its password, and a HEAD
// of a collection link answer as documented.
func TestHandlerEdges(t *testing.T) {
	ctx := t.Context()
	u := newSuite(t, 0)
	f := u.file(t, "a.txt")
	dir, err := u.folders.Create(ctx, u.store, "t", folders.RootID, "docs")
	if err != nil {
		t.Fatal(err)
	}
	fileLink, err := u.plugin.Create(ctx, u.store, "t", f.ID, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	locked, err := u.plugin.CreateFor(ctx, u.store, "t", share.Target{Kind: folders.KindFolder, Ref: dir.ID}, share.Options{TTL: time.Hour, Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	open, err := u.plugin.CreateFor(ctx, u.store, "t", share.Target{Kind: folders.KindFolder, Ref: dir.ID}, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := share.Handler(u.store, u.plugin)
	for _, c := range []struct {
		name, method, path string
		want               int
	}{
		{"file path under a file link", http.MethodGet, "/" + fileLink.Token + "/" + f.ID, http.StatusNotFound},
		{"POST to a file link without a password", http.MethodPost, "/" + fileLink.Token, http.StatusNotFound},
		{"collection without its password", http.MethodGet, "/" + locked.Token, http.StatusUnauthorized},
		{"HEAD of a collection", http.MethodHead, "/" + open.Token, http.StatusNoContent},
	} {
		if rec := serve(h, c.method, c.path, nil, nil); rec.Code != c.want {
			t.Fatalf("%s = %d, want %d", c.name, rec.Code, c.want)
		}
	}
	if uses := u.uses(t, fileLink); uses != 0 {
		t.Fatalf("refused requests spent %d uses", uses)
	}
}

// formPart is one part of a multipart body; a part with a filename is a
// file.
type formPart struct{ field, filename, body string }

func multipartBody(t *testing.T, parts ...formPart) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		var w io.Writer
		var err error
		if p.filename == "" {
			w, err = mw.CreateFormField(p.field)
		} else {
			w, err = mw.CreateFormFile(p.field, p.filename)
		}
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, p.body)
	}
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

// TestDepositAnswers: a request link's upload answers 400 for a body that
// is not a well-formed multipart form, 401 for a wrong password given as
// a form field, 429 once the limiter refuses, 413 for a file over the
// policy, 415 for a type it refuses and 500 when the database fails; no
// refused upload keeps a file.
func TestDepositAnswers(t *testing.T) {
	ctx := t.Context()
	u := newSuite(t, 0)
	post := records.Owner{Type: "post", ID: "p1"}
	target := share.Target{Kind: share.KindOwner, Ref: share.OwnerRef(post, "inbox")}
	sh, err := u.plugin.CreateRequest(ctx, u.store, "t", target, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	guarded, err := u.plugin.CreateRequest(ctx, u.store, "t", target, share.Options{TTL: time.Hour, Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	pngOnly := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"image/png"}}
	h := share.Handler(u.store, u.plugin, share.WithUploads("media", anyText))
	strict := share.Handler(u.store, u.plugin, share.WithUploads("media", pngOnly))
	closed := share.Handler(u.store, u.plugin, share.WithUploads("media", anyText), share.WithAttemptLimiter(share.NewAttemptLimiter(0, time.Minute)))
	post1 := func(h http.Handler, token string, body *bytes.Buffer, ct string) int {
		return serve(h, http.MethodPost, "/"+token, body, http.Header{"Content-Type": {ct}}).Code
	}
	form := func(parts ...formPart) (*bytes.Buffer, string) { return multipartBody(t, parts...) }

	if code := post1(h, sh.Token, bytes.NewBufferString("x=1"), "application/x-www-form-urlencoded"); code != http.StatusBadRequest {
		t.Fatalf("a form that is not multipart = %d", code)
	}
	b, ct := form(formPart{field: "note", body: "no file"})
	if code := post1(h, sh.Token, b, ct); code != http.StatusBadRequest {
		t.Fatalf("a form without a file = %d", code)
	}
	b, ct = form(formPart{field: share.FormPassword, body: "pw"})
	cut := bytes.NewBuffer(b.Bytes()[:b.Len()-len("--\r\n")-30])
	if code := post1(h, guarded.Token, cut, ct); code != http.StatusBadRequest {
		t.Fatalf("a body cut inside the password = %d", code)
	}
	b, ct = form(formPart{field: share.FormPassword, body: "wrong"}, formPart{field: share.FormFile, filename: "a.txt", body: "a"})
	if code := post1(h, guarded.Token, b, ct); code != http.StatusUnauthorized {
		t.Fatalf("a wrong password field = %d", code)
	}
	b, ct = form(formPart{field: share.FormPassword, body: "pw"}, formPart{field: share.FormFile, filename: "a.txt", body: "a"})
	if code := post1(closed, guarded.Token, b, ct); code != http.StatusTooManyRequests {
		t.Fatalf("a password past the limiter = %d", code)
	}
	b, ct = form(formPart{field: share.FormFile, filename: "big.txt", body: strings.Repeat("b", 2<<10)})
	if code := post1(h, sh.Token, b, ct); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a file over the policy = %d", code)
	}
	b, ct = form(formPart{field: share.FormFile, filename: "a.txt", body: "plain text"})
	if code := post1(strict, sh.Token, b, ct); code != http.StatusUnsupportedMediaType {
		t.Fatalf("a refused type = %d", code)
	}
	testdb.OnlySQLite(t)
	testdb.Exec(t, u.db, "CREATE TRIGGER fail_insert BEFORE INSERT ON "+records.TableNameFiles+" BEGIN SELECT RAISE(FAIL, 'injected'); END")
	b, ct = form(formPart{field: share.FormFile, filename: "a.txt", body: "a"})
	if code := post1(h, sh.Token, b, ct); code != http.StatusInternalServerError {
		t.Fatalf("a deposit the database refuses = %d", code)
	}
	if page, _ := u.store.Files(ctx, records.FileQuery{Scope: "t"}); len(page.Files) != 0 {
		t.Fatalf("refused uploads kept %d files", len(page.Files))
	}
}

// TestCollectionFaultSweep fails every database step of the collection
// and request link paths and of the plugin's cleanup in turn: each
// failure is the injected fault, returned, and the run after the sweep
// succeeds.
func TestCollectionFaultSweep(t *testing.T) {
	ctx := t.Context()
	u := newSuite(t, 0)
	f := u.file(t, "a.txt")
	dir, err := u.folders.Create(ctx, u.store, "t", folders.RootID, "docs")
	if err != nil {
		t.Fatal(err)
	}
	if err := u.folders.Put(ctx, u.store, "t", f.ID, dir.ID); err != nil {
		t.Fatal(err)
	}
	dirLink, err := u.plugin.CreateFor(ctx, u.store, "t", share.Target{Kind: folders.KindFolder, Ref: dir.ID}, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	post := records.Owner{Type: "post", ID: "p1"}
	req, err := u.plugin.CreateRequest(ctx, u.store, "t", share.Target{Kind: share.KindOwner, Ref: share.OwnerRef(post, "inbox")}, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ownerLink, err := u.plugin.CreateFor(ctx, u.store, "t", share.Target{Kind: share.KindOwner, Ref: share.OwnerRef(post, "inbox")}, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ops := []struct {
		name string
		run  func() error
	}{
		{"check file", func() error { _, err := u.plugin.CheckFile(ctx, u.store, dirLink.Token, f.ID); return err }},
		{"resolve file", func() error { _, err := u.plugin.ResolveFile(ctx, u.store, dirLink.Token, f.ID); return err }},
		{"stats", func() error { _, err := u.plugin.Stats(ctx, u.store, "t", dirLink.ID, 10); return err }},
		{"deposit", func() error {
			_, err := u.plugin.Deposit(ctx, u.store, req.Token, "media", strings.NewReader("d"), "d.txt", anyText)
			return err
		}},
		{"owner files", func() error { _, _, err := u.plugin.List(ctx, u.store, ownerLink.Token); return err }},
		{"owner contains", func() error {
			// f is not attached to the post: once nothing fails, the answer is gone.
			_, err := u.plugin.CheckFile(ctx, u.store, ownerLink.Token, f.ID)
			if !errors.Is(err, testdb.ErrInjected) {
				gone(t, err)
				return nil
			}
			return err
		}},
		{"purge", func() error { return u.store.Purge(ctx, "t", f.ID) }},
		{"prune", func() error { _, err := u.plugin.Prune(ctx, u.store, time.Now().Add(2*time.Hour)); return err }},
		{"purge scope", func() error { _, err := u.store.PurgeScope(ctx, "t"); return err }},
		// A scope holding links and no files: every step is the plugin's
		// own cleanup, which a scope with files reaches only after its
		// purges committed.
		{"purge a scope of links only", func() error { _, err := u.store.PurgeScope(ctx, "links"); return err }},
	}
	if _, err := u.plugin.CreateFor(ctx, u.store, "links", share.Target{Kind: share.KindOwner, Ref: share.OwnerRef(post, "inbox")}, share.Options{TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			for n := 1; ; n++ {
				u.faults.Arm(n)
				err := op.run()
				u.faults.Arm(0)
				if err == nil {
					return
				}
				if !errors.Is(err, testdb.ErrInjected) {
					t.Fatalf("step %d: %v", n, err)
				}
			}
		})
	}
}
