package recordshttp_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordshttp"
	"github.com/ubgo/filesystem/upload"
)

// testPolicy is a direct Replace's policy in the race test.
var testPolicy = upload.Policy{MaxSize: 1 << 10, Allowed: []string{"text/plain"}}

// doIfMatch is e.do with an If-Match header.
func (e *env) doIfMatch(method, path, ifMatch string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	req = req.WithContext(records.WithActor(req.Context(), "user:t"))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("If-Match", ifMatch)
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	return rec
}

// TestIfMatch: PUT /files/{id} with the ETag the client read replaces and
// answers the new ETag; a stale ETag is 412 and changes nothing; "*" and
// a list holding the current ETag pass; visibility and trash honour it
// the same way.
func TestIfMatch(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	af := decode[recordshttp.ListResponse](t, e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "first"})).Files[0]
	file := func() records.File {
		f, err := e.store.File(ctx, tenant, af.FileID)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	read := `"` + file().SHA256 + `"`
	put := func(ifMatch, content string) *httptest.ResponseRecorder {
		body, ct := form(t, part{field: recordshttp.FormField, filename: "a.txt", body: content})
		return e.doIfMatch(http.MethodPut, "/files/"+af.FileID, ifMatch, body, ct)
	}

	rec := put(read, "second")
	wantStatus(t, rec, http.StatusOK)
	current := file()
	if rec.Header().Get("ETag") != `"`+current.SHA256+`"` || current.SHA256 == strings.Trim(read, `"`) {
		t.Fatalf("ETag after the replace = %q, file %s", rec.Header().Get("ETag"), current.SHA256)
	}
	objects := e.objects(t)
	wantError(t, put(read, "stale save"), http.StatusPreconditionFailed, filesystem.CodeConflict, "changed")
	if file().SHA256 != current.SHA256 || e.objects(t) != objects {
		t.Fatal("a stale If-Match changed the file or stored its body")
	}
	wantStatus(t, put(`W/"nope", "`+current.SHA256+`"`, "third"), http.StatusOK)
	wantStatus(t, put("*", "fourth"), http.StatusOK)

	vis := func(ifMatch string) *httptest.ResponseRecorder {
		return e.doIfMatch(http.MethodPut, "/files/"+af.FileID+"/visibility", ifMatch, strings.NewReader(`{"visibility":"public"}`), "application/json")
	}
	wantError(t, vis(read), http.StatusPreconditionFailed, filesystem.CodeConflict, "changed")
	if file().Visibility != filesystem.VisibilityPrivate {
		t.Fatal("a stale If-Match changed the visibility")
	}
	wantStatus(t, vis(`"`+file().SHA256+`"`), http.StatusNoContent)

	wantError(t, e.doIfMatch(http.MethodDelete, "/files/"+af.FileID, read, nil, ""), http.StatusPreconditionFailed, filesystem.CodeConflict, "changed")
	if file().DeletedAt != nil {
		t.Fatal("a stale If-Match trashed the file")
	}
	wantStatus(t, e.doIfMatch(http.MethodDelete, "/files/"+af.FileID, `"`+file().SHA256+`"`, nil, ""), http.StatusNoContent)
}

// TestIfMatchRace: a replacement landing between the route's check and
// its write is still refused with 412: the Store checks again under the
// row lock (IfUnchanged).
func TestIfMatchRace(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	af := decode[recordshttp.ListResponse](t, e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "first"})).Files[0]
	f, err := e.store.File(ctx, tenant, af.FileID)
	if err != nil {
		t.Fatal(err)
	}
	e.onAuth = func() {
		e.onAuth = nil // once: the replacement the request races
		if _, err := e.store.Replace(ctx, tenant, f.ID, strings.NewReader("concurrent"), "", testPolicy); err != nil {
			t.Error(err)
		}
	}
	body, ct := form(t, part{field: recordshttp.FormField, filename: "a.txt", body: "mine"})
	rec := e.doIfMatch(http.MethodPut, "/files/"+f.ID, `"`+f.SHA256+`"`, body, ct)
	wantError(t, rec, http.StatusPreconditionFailed, filesystem.CodeConflict, "changed")
}

// TestReadEphemeral: GET /files/{id} serves a view-once file once and
// answers 404 after; an expired file is 404 too.
func TestReadEphemeral(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	once, err := e.store.Upload(ctx, tenant, "media", strings.NewReader("secret"), "s.txt", testPolicy, records.ViewOnce())
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, e.do(http.MethodGet, "/files/"+once.ID, nil, ""), http.StatusFound)
	wantError(t, e.do(http.MethodGet, "/files/"+once.ID, nil, ""), http.StatusNotFound, filesystem.CodeNotFound, "")
	gone, err := e.store.Upload(ctx, tenant, "media", strings.NewReader("story"), "story.txt", testPolicy, records.Expires(time.Now().Add(-time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	wantError(t, e.do(http.MethodGet, "/files/"+gone.ID, nil, ""), http.StatusNotFound, filesystem.CodeNotFound, "")
}

// TestIfMatchRacePerRoute: on every guarded route, a replacement landing
// after the route loaded the file and before it changes it is refused
// with 412 by the Store's check under the row lock, and the file is left
// as the replacement made it.
func TestIfMatchRacePerRoute(t *testing.T) {
	for _, c := range []struct {
		name    string
		method  string
		path    string
		body    string
		trashed bool
		check   func(f records.File) bool // the route did not act
	}{
		{"visibility", http.MethodPut, "/visibility", `{"visibility":"public"}`, false, func(f records.File) bool { return f.Visibility == filesystem.VisibilityPrivate }},
		{"trash", http.MethodDelete, "", "", false, func(f records.File) bool { return f.DeletedAt == nil }},
		{"restore", http.MethodPost, "/restore", "", true, func(f records.File) bool { return f.DeletedAt != nil }},
		{"purge", http.MethodPost, "/purge", "", false, func(f records.File) bool { return true }},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := t.Context()
			e := newEnv(t)
			f, err := e.store.Upload(ctx, tenant, "media", strings.NewReader("first"), "a.txt", testPolicy)
			if err != nil {
				t.Fatal(err)
			}
			if c.trashed {
				if err := e.store.Trash(ctx, tenant, f.ID); err != nil {
					t.Fatal(err)
				}
			}
			e.onAuth = func() {
				e.onAuth = nil // the change the request races, once
				if c.trashed {
					_ = e.store.Restore(ctx, tenant, f.ID)
				}
				if _, err := e.store.Replace(ctx, tenant, f.ID, strings.NewReader("concurrent"), "", testPolicy); err != nil {
					t.Error(err)
				}
				if c.trashed {
					_ = e.store.Trash(ctx, tenant, f.ID)
				}
			}
			var body io.Reader
			ct := ""
			if c.body != "" {
				body, ct = strings.NewReader(c.body), "application/json"
			}
			rec := e.doIfMatch(c.method, "/files/"+f.ID+c.path, `"`+f.SHA256+`"`, body, ct)
			wantError(t, rec, http.StatusPreconditionFailed, filesystem.CodeConflict, "changed")
			after, err := e.store.File(ctx, tenant, f.ID)
			if err != nil || !c.check(after) {
				t.Fatalf("the refused %s acted: %+v, %v", c.name, after, err)
			}
		})
	}
}

// TestListEphemeral: GET /owners/… leaves expired and viewed view-once
// files out, and lists an unviewed view-once file without a link, so the
// listing neither serves past an expiry nor spends the one view.
func TestListEphemeral(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	post := records.Owner{Type: "post", ID: "p1"}
	attach := func(name string, opts ...records.UploadOption) records.File {
		f, err := e.store.Upload(ctx, tenant, "media", strings.NewReader(name), name, testPolicy, opts...)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.store.Attach(ctx, tenant, f.ID, post, "gallery"); err != nil {
			t.Fatal(err)
		}
		return f
	}
	plain := attach("plain.txt")
	once := attach("once.txt", records.ViewOnce())
	attach("gone.txt", records.Expires(time.Now().Add(-time.Minute)))
	links := func() map[string]string {
		out := map[string]string{}
		for _, af := range e.list(t, "p1") {
			out[af.FileID] = af.URL
		}
		return out
	}
	got := links()
	if len(got) != 2 || got[plain.ID] == "" || got[once.ID] != "" {
		t.Fatalf("listing = %v", got)
	}
	wantStatus(t, e.do(http.MethodGet, "/files/"+once.ID, nil, ""), http.StatusFound)
	if got := links(); len(got) != 1 || got[plain.ID] == "" {
		t.Fatalf("listing after the one view = %v", got)
	}
}
