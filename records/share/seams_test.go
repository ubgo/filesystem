package share_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/share"
	"github.com/ubgo/filesystem/upload"
)

// lifetimeSigner records the lifetime of the last URL it signed.
type lifetimeSigner struct{ expires *time.Duration }

func (s lifetimeSigner) SignURL(_ context.Context, disk, key string, o filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	*s.expires = o.Expires
	return filesystem.SignedRequest{Method: o.Method, URL: "https://app.test/" + disk + "/" + key}, nil
}

type orgKey struct{}

// seam is a Store with the share plugin built from opts, a signed disk
// whose last link lifetime is in expires, and one file.
type seam struct {
	store   *records.Store
	plugin  *share.Plugin
	file    records.File
	expires time.Duration
}

func newSeam(t *testing.T, storeOpts []records.Option, opts ...share.Option) *seam {
	t.Helper()
	u := &seam{}
	db, _ := testdb.OpenFaulty(t)
	disk, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithURLSigner(lifetimeSigner{&u.expires}))
	mgr, _ := filesystem.NewManager(disk)
	u.plugin = share.New(append([]share.Option{share.WithCollections(share.Owners{})}, opts...)...)
	var err error
	u.store, err = records.New(db, testdb.Dialect(), mgr, append([]records.Option{records.WithPlugins(u.plugin), records.WithOwnerTypes("post")}, storeOpts...)...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	u.file, err = u.store.Upload(t.Context(), "t", "media", strings.NewReader("hello"), "a.txt", anyText)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (u *seam) link(t *testing.T, o share.Options) share.Share {
	t.Helper()
	sh, err := u.plugin.Create(t.Context(), u.store, "t", u.file.ID, o)
	if err != nil {
		t.Fatal(err)
	}
	return sh
}

func (u *seam) uses(t *testing.T, sh share.Share) int64 {
	t.Helper()
	st, err := u.plugin.Stats(t.Context(), u.store, "t", sh.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	return st.Uses
}

var hour = share.Options{TTL: time.Hour}

// TestTokenBytes (I7): tokens are 256 bits by default; WithTokenBytes can
// lengthen them and the longer token works; anything below the floor or
// past the bound fails the plugin closed.
func TestTokenBytes(t *testing.T) {
	d := newSeam(t, nil)
	if got := len(d.link(t, hour).Token); got != 43 {
		t.Fatalf("default token is %d characters, want 43 (32 bytes)", got)
	}
	long := newSeam(t, nil, share.WithTokenBytes(48))
	sh := long.link(t, hour)
	if len(sh.Token) != 64 {
		t.Fatalf("48-byte token is %d characters", len(sh.Token))
	}
	if _, err := long.plugin.Resolve(t.Context(), long.store, sh.Token); err != nil {
		t.Fatalf("a long token must resolve: %v", err)
	}
	for _, n := range []int{0, 16, share.MinTokenBytes - 1, share.MaxTokenBytes + 1} {
		p := share.New(share.WithTokenBytes(n))
		if _, err := p.Migrations(records.EngineSQLite); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%d bytes: Migrations = %v", n, err)
		}
		if _, err := p.Create(t.Context(), d.store, "t", d.file.ID, hour); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%d bytes: Create = %v", n, err)
		}
	}
}

// TestMaxLifetime (C6): WithMaxLifetime lowers the cap; it cannot raise it
// past MaxLifetime; the default still allows MaxLifetime.
func TestMaxLifetime(t *testing.T) {
	u := newSeam(t, nil, share.WithMaxLifetime(24*time.Hour))
	if _, err := u.plugin.Create(t.Context(), u.store, "t", u.file.ID, share.Options{TTL: 48 * time.Hour}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a TTL past the lowered cap: %v", err)
	}
	u.link(t, share.Options{TTL: 12 * time.Hour})
	d := newSeam(t, nil)
	d.link(t, share.Options{TTL: share.MaxLifetime})
	for _, bad := range []time.Duration{0, -time.Hour, share.MaxLifetime + time.Hour} {
		p := share.New(share.WithMaxLifetime(bad))
		if _, err := p.Create(t.Context(), d.store, "t", d.file.ID, hour); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("max lifetime %s: Create = %v", bad, err)
		}
	}
}

// statusWriter is an ErrorWriter that answers "custom <status>" and keeps
// every cause.
func statusWriter(causes *[]error) fshttp.ErrorWriter {
	return func(w http.ResponseWriter, _ *http.Request, status int, err error) {
		*causes = append(*causes, err)
		w.WriteHeader(status)
		_, _ = w.Write([]byte("custom " + http.StatusText(status)))
	}
}

// TestHandlerErrors (B5): WithErrors answers every refusal with the
// application's writer, given ErrPassword for the 401 (where a password
// page goes) and a not-found error for every unusable link; without it the
// plain answers are unchanged.
func TestHandlerErrors(t *testing.T) {
	u := newSeam(t, nil)
	locked := u.link(t, share.Options{TTL: time.Hour, Password: "s3cret"})
	var causes []error
	h := share.Handler(u.store, u.plugin, share.WithErrors(statusWriter(&causes)))
	for _, c := range []struct {
		method, path string
		status       int
		check        func(error) bool
	}{
		{http.MethodGet, "/unknown", http.StatusNotFound, func(err error) bool { return filesystem.CodeOf(err) == filesystem.CodeNotFound }},
		{http.MethodGet, "/" + locked.Token, http.StatusUnauthorized, func(err error) bool { return errors.Is(err, share.ErrPassword) }},
		{http.MethodDelete, "/" + locked.Token, http.StatusMethodNotAllowed, func(err error) bool { return err != nil }},
	} {
		rec := serve(h, c.method, c.path, nil, nil)
		if rec.Code != c.status || !strings.HasPrefix(rec.Body.String(), "custom ") {
			t.Fatalf("%s %s = %d %q", c.method, c.path, rec.Code, rec.Body)
		}
		if last := causes[len(causes)-1]; !c.check(last) {
			t.Fatalf("%s %s: cause %v", c.method, c.path, last)
		}
	}

	plain := share.Handler(u.store, u.plugin)
	if rec := serve(plain, http.MethodGet, "/unknown", nil, nil); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "404 page not found") {
		t.Fatalf("default 404 = %d %q", rec.Code, rec.Body)
	}
	if rec := serve(plain, http.MethodGet, "/"+locked.Token, nil, nil); rec.Code != http.StatusUnauthorized || strings.TrimSpace(rec.Body.String()) != "password required" {
		t.Fatalf("default 401 = %d %q", rec.Code, rec.Body)
	}
}

// TestLinkLifetime (I8): the signed URL lives DefaultLinkLifetime unless
// WithLinkLifetime says otherwise; a lifetime outside (0,
// MaxLinkLifetime] makes the handler answer 500 to everything.
func TestLinkLifetime(t *testing.T) {
	u := newSeam(t, nil)
	sh := u.link(t, share.Options{TTL: time.Hour})
	if rec := serve(share.Handler(u.store, u.plugin), http.MethodGet, "/"+sh.Token, nil, nil); rec.Code != http.StatusFound || u.expires != share.DefaultLinkLifetime {
		t.Fatalf("default = %d, lifetime %s", rec.Code, u.expires)
	}
	if rec := serve(share.Handler(u.store, u.plugin, share.WithLinkLifetime(30*time.Minute)), http.MethodGet, "/"+sh.Token, nil, nil); rec.Code != http.StatusFound || u.expires != 30*time.Minute {
		t.Fatalf("custom = %d, lifetime %s", rec.Code, u.expires)
	}
	before := u.uses(t, sh)
	for _, bad := range []time.Duration{0, share.MaxLinkLifetime + time.Second} {
		if rec := serve(share.Handler(u.store, u.plugin, share.WithLinkLifetime(bad)), http.MethodGet, "/"+sh.Token, nil, nil); rec.Code != http.StatusInternalServerError {
			t.Fatalf("lifetime %s = %d", bad, rec.Code)
		}
	}
	if u.uses(t, sh) != before {
		t.Fatal("a misconfigured handler spent a use")
	}
}

// TestDeliver (I8): WithDeliver hands the file over instead of the
// redirect, with the signed URL and the link, after the use is spent.
func TestDeliver(t *testing.T) {
	u := newSeam(t, nil)
	sh := u.link(t, share.Options{TTL: time.Hour})
	var got share.Share
	h := share.Handler(u.store, u.plugin, share.WithDeliver(func(w http.ResponseWriter, _ *http.Request, link share.Share, f records.File, url string) {
		got = link
		_, _ = w.Write([]byte("<a href=\"" + url + "\">" + f.Filename + "</a>"))
	}))
	rec := serve(h, http.MethodGet, "/"+sh.Token, nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "https://app.test/media/") || got.ID != sh.ID {
		t.Fatalf("deliver = %d %q", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("headers: %v", rec.Header())
	}
	if u.uses(t, sh) != 1 {
		t.Fatal("a delivered download must spend one use")
	}
}

// TestAccessCheck (I9): WithAccessCheck admits only who it allows, before
// the password and before anything is spent; its error's code is the
// status (CodeNotFound hides the link, an uncoded error is 500); an
// unusable link never reaches it and stays 404.
func TestAccessCheck(t *testing.T) {
	u := newSeam(t, nil)
	sh := u.link(t, share.Options{TTL: time.Hour})
	calls := 0
	var hide, broken bool
	h := share.Handler(u.store, u.plugin, share.WithAccessCheck(func(r *http.Request, link share.Share) error {
		calls++
		switch {
		case broken:
			return errors.New("directory down")
		case hide:
			return filesystem.Errorf(filesystem.CodeNotFound, "not for you")
		case r.Context().Value(orgKey{}) != "acme" || link.Scope != "t":
			return filesystem.Errorf(filesystem.CodePermission, "sign in to acme")
		}
		return nil
	}))
	get := func() *httptest.ResponseRecorder { return serve(h, http.MethodGet, "/"+sh.Token, nil, nil) }
	if rec := get(); rec.Code != http.StatusForbidden || u.uses(t, sh) != 0 {
		t.Fatalf("outsider = %d, uses %d", rec.Code, u.uses(t, sh))
	}
	hide = true
	if rec := get(); rec.Code != http.StatusNotFound {
		t.Fatalf("hidden = %d", rec.Code)
	}
	hide, broken = false, true
	if rec := get(); rec.Code != http.StatusInternalServerError || u.uses(t, sh) != 0 {
		t.Fatalf("failing check = %d", rec.Code)
	}
	broken = false
	r := httptest.NewRequest(http.MethodGet, "/"+sh.Token, nil)
	r = r.WithContext(context.WithValue(r.Context(), orgKey{}, "acme"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusFound || u.uses(t, sh) != 1 {
		t.Fatalf("member = %d, uses %d", rec.Code, u.uses(t, sh))
	}
	if err := u.plugin.Revoke(t.Context(), u.store, "t", sh.ID); err != nil {
		t.Fatal(err)
	}
	before := calls
	if rec := get(); rec.Code != http.StatusNotFound || calls != before {
		t.Fatalf("revoked link = %d, check called %d more times", rec.Code, calls-before)
	}
}

// TestUploadPolicy (I9): WithUploadPolicy chooses each request link's
// policy from the link; its error refuses with its status, an invalid
// policy is 500, and without it WithUploads' policy applies as before.
func TestUploadPolicy(t *testing.T) {
	u := newSeam(t, nil)
	post := records.Owner{Type: "post", ID: "p1"}
	req, err := u.plugin.CreateRequest(t.Context(), u.store, "t", share.Target{Kind: share.KindOwner, Ref: share.OwnerRef(post, "inbox")}, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	var seen string
	var refuse, invalid bool
	resolver := func(_ *http.Request, link share.Share, base upload.Policy) (upload.Policy, error) {
		seen = link.ID
		switch {
		case refuse:
			return upload.Policy{}, filesystem.Errorf(filesystem.CodePermission, "uploads closed")
		case invalid:
			return upload.Policy{}, nil
		}
		base.MaxSize = 5
		return base, nil
	}
	send := func(h http.Handler, body string) int {
		buf, ct := multipartBody(t, formPart{"file", "doc.txt", body})
		return serve(h, http.MethodPost, "/"+req.Token, buf, http.Header{"Content-Type": {ct}}).Code
	}
	h := share.Handler(u.store, u.plugin, share.WithUploads("media", anyText), share.WithUploadPolicy(resolver))
	if code := send(h, "0123456789"); code != http.StatusRequestEntityTooLarge || seen != req.ID {
		t.Fatalf("10 bytes under a 5-byte link policy = %d (resolver saw %q)", code, seen)
	}
	if code := send(h, "abc"); code != http.StatusCreated {
		t.Fatalf("3 bytes = %d", code)
	}
	refuse = true
	if code := send(h, "abc"); code != http.StatusForbidden {
		t.Fatalf("refusing resolver = %d", code)
	}
	refuse, invalid = false, true
	if code := send(h, "abc"); code != http.StatusInternalServerError {
		t.Fatalf("invalid policy = %d", code)
	}
	if code := send(share.Handler(u.store, u.plugin, share.WithUploads("media", anyText)), "0123456789"); code != http.StatusCreated {
		t.Fatalf("default policy = %d", code)
	}
}

// TestViewerFromStoreActor (I3): the share plugin records the viewer the
// Store's actor lookup names (records.WithActorFunc), not only WithActor.
func TestViewerFromStoreActor(t *testing.T) {
	u := newSeam(t, []records.Option{records.WithActorFunc(func(ctx context.Context) string {
		if org, _ := ctx.Value(orgKey{}).(string); org != "" {
			return "member:" + org
		}
		return ""
	})})
	sh := u.link(t, share.Options{TTL: time.Hour})
	ctx := context.WithValue(t.Context(), orgKey{}, "acme")
	if _, err := u.plugin.Resolve(ctx, u.store, sh.Token); err != nil {
		t.Fatal(err)
	}
	st, err := u.plugin.Stats(t.Context(), u.store, "t", sh.ID, 1)
	if err != nil || len(st.Recent) != 1 || st.Recent[0].Viewer != "member:acme" {
		t.Fatalf("stats = %+v, %v", st, err)
	}
}
