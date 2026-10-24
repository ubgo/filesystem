package recordshttp_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/recordshttp"
	"github.com/ubgo/filesystem/upload"
)

const (
	tenant = "t1"
	pngSig = "\x89PNG\r\n\x1a\n"
)

// signer signs every key with a fixed URL and records the lifetime asked
// for.
type signer struct{ expires *time.Duration }

func (s signer) SignURL(_ context.Context, disk, key string, o filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	*s.expires = o.Expires
	return filesystem.SignedRequest{Method: o.Method, URL: "https://cdn.test/" + disk + "/" + key}, nil
}

// call is one Authorize call.
type call struct {
	action recordshttp.Action
	target recordshttp.Target
}

type env struct {
	db      *sql.DB
	store   *records.Store
	disk    *filesystem.Disk
	srv     http.Handler
	calls   []call
	deny    recordshttp.Action // Authorize refuses this action
	anon    bool               // Scope refuses every request
	tenant  string             // the scope requests act in; "" means tenant
	onScope func()             // runs inside Scope
	onAuth  func()             // runs inside Authorize
	expires time.Duration
}

type option func(*recordshttp.Config, *[]filesystem.Option)

func newEnv(t *testing.T, opts ...option) *env {
	t.Helper()
	return newEnvWith(t, nil, opts...)
}

// newEnvWith is newEnv with plugins installed in the Store.
func newEnvWith(t *testing.T, plugins []records.Plugin, opts ...option) *env {
	t.Helper()
	e := &env{db: testdb.Open(t)}
	cfg := recordshttp.Config{
		Disk:   "media",
		Policy: upload.Policy{MaxSize: 1 << 10, Allowed: []string{"text/plain", "image/png"}},
		Scope: func(*http.Request) (string, error) {
			if e.onScope != nil {
				e.onScope()
			}
			if e.anon {
				return "", errors.New("no session")
			}
			if e.tenant != "" {
				return e.tenant, nil
			}
			return tenant, nil
		},
		Authorize: func(_ *http.Request, a recordshttp.Action, tg recordshttp.Target) error {
			e.calls = append(e.calls, call{a, tg})
			if e.onAuth != nil {
				e.onAuth()
			}
			if a == e.deny {
				return errors.New("not your post")
			}
			return nil
		},
	}
	diskOpts := []filesystem.Option{filesystem.WithURLSigner(signer{&e.expires})}
	for _, o := range opts {
		o(&cfg, &diskOpts)
	}
	e.disk, _ = filesystem.NewDisk("media", memory.New(), diskOpts...)
	mgr, _ := filesystem.NewManager(e.disk)
	var err error
	e.store, err = records.New(e.db, testdb.Dialect(), mgr, records.WithOwnerTypes("post"), records.WithPlugins(plugins...))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	cfg.Store = e.store
	if e.srv, err = recordshttp.Handler(cfg); err != nil {
		t.Fatal(err)
	}
	return e
}

func noSigner(_ *recordshttp.Config, o *[]filesystem.Option) { *o = nil }

// part is one multipart form part; a part without a filename is a plain
// field.
type part struct{ field, filename, body string }

func form(t *testing.T, parts ...part) (io.Reader, string) {
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

func (e *env) do(method, path string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	req = req.WithContext(records.WithActor(req.Context(), "user:t")) // what the app's auth middleware does
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	return rec
}

func (e *env) upload(t *testing.T, owner string, parts ...part) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := form(t, parts...)
	return e.do(http.MethodPost, "/owners/post/"+owner+"/gallery", body, ct)
}

func decode[T recordshttp.ListResponse | recordshttp.FileResponse | recordshttp.AttachedFile | recordshttp.ErrorResponse | recordshttp.StatusResponse](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, status, rec.Body.String())
	}
}

func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code filesystem.Code, msg string) {
	t.Helper()
	wantStatus(t, rec, status)
	er := decode[recordshttp.ErrorResponse](t, rec)
	if er.Code != code || !strings.Contains(er.Error, msg) {
		t.Fatalf("error = %+v, want code %s containing %q", er, code, msg)
	}
}

// objects counts the stored objects.
func (e *env) objects(t *testing.T) int {
	t.Helper()
	page, err := e.disk.ListPage(t.Context(), "", filesystem.ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	return len(page.Entries)
}

func (e *env) list(t *testing.T, owner string) []recordshttp.AttachedFile {
	t.Helper()
	rec := e.do(http.MethodGet, "/owners/post/"+owner+"/gallery", nil, "")
	wantStatus(t, rec, http.StatusOK)
	return decode[recordshttp.ListResponse](t, rec).Files
}

func TestHandlerConfig(t *testing.T) {
	scope := func(*http.Request) (string, error) { return "", nil }
	auth := func(*http.Request, recordshttp.Action, recordshttp.Target) error { return nil }
	store := &records.Store{}
	policy := upload.Policy{MaxSize: 1, Allowed: []string{"image/*"}}
	for name, cfg := range map[string]recordshttp.Config{
		"no store":     {Disk: "d", Policy: policy, Scope: scope, Authorize: auth},
		"no disk":      {Store: store, Policy: policy, Scope: scope, Authorize: auth},
		"no scope":     {Store: store, Disk: "d", Policy: policy, Authorize: auth},
		"no authorize": {Store: store, Disk: "d", Policy: policy, Scope: scope},
		"bad policy":   {Store: store, Disk: "d", Scope: scope, Authorize: auth},
	} {
		if _, err := recordshttp.Handler(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestUploadAndList(t *testing.T) {
	e := newEnv(t)
	rec := e.upload(t, "p1",
		part{field: "caption", body: "skipped: not a file"},
		part{field: recordshttp.FormField, body: "skipped: a plain value, not a file"},
		part{field: recordshttp.FormField, filename: "a.txt", body: "first"},
		part{field: "other", filename: "x.txt", body: "skipped: wrong field"},
		part{field: recordshttp.FormField, filename: "b.png", body: pngSig + "pixels"},
	)
	wantStatus(t, rec, http.StatusCreated)
	got := decode[recordshttp.ListResponse](t, rec).Files
	if len(got) != 2 || got[0].Filename != "a.txt" || got[1].ContentType != "image/png" || got[1].Size != int64(len(pngSig)+6) || got[0].Position >= got[1].Position {
		t.Fatalf("uploaded = %+v", got)
	}
	want := call{recordshttp.ActionUpload, recordshttp.Target{Scope: tenant, Owner: records.Owner{Type: "post", ID: "p1"}, Role: "gallery"}}
	if e.calls[0] != want {
		t.Fatalf("Authorize got %+v, want %+v", e.calls[0], want)
	}

	listed := e.list(t, "p1")
	if len(listed) != 2 || listed[0].AttachmentID != got[0].AttachmentID || listed[0].UploadedBy != "user:t" || !strings.HasPrefix(listed[1].URL, "https://cdn.test/media/") {
		t.Fatalf("listed = %+v", listed)
	}
	if e.expires != recordshttp.DefaultLinkLifetime {
		t.Fatalf("link lifetime = %v", e.expires)
	}
	if len(e.list(t, "other")) != 0 {
		t.Fatal("another owner sees the files")
	}
}

func TestUploadIsAllOrNothing(t *testing.T) {
	e := newEnv(t)
	rec := e.upload(t, "p1",
		part{field: recordshttp.FormField, filename: "ok.txt", body: "fine"},
		part{field: recordshttp.FormField, filename: "page.html", body: "<!DOCTYPE html><script>alert(1)</script>"},
	)
	wantError(t, rec, http.StatusUnsupportedMediaType, filesystem.CodeRejected, "text/html")
	if n := e.objects(t); n != 0 {
		t.Fatalf("%d objects left after a failed upload", n)
	}
	if len(e.list(t, "p1")) != 0 {
		t.Fatal("rows left after a failed upload")
	}
}

func TestUploadRefusals(t *testing.T) {
	e := newEnv(t, func(c *recordshttp.Config, _ *[]filesystem.Option) { c.MaxFiles = 1 })
	cases := []struct {
		name   string
		rec    func() *httptest.ResponseRecorder
		status int
		code   filesystem.Code
		msg    string
	}{
		{"not multipart", func() *httptest.ResponseRecorder {
			return e.do(http.MethodPost, "/owners/post/p1/gallery", strings.NewReader("{}"), "application/json")
		}, http.StatusBadRequest, filesystem.CodeInvalidArgument, "multipart"},
		{"no file", func() *httptest.ResponseRecorder {
			return e.upload(t, "p1", part{field: "caption", body: "hi"})
		}, http.StatusBadRequest, filesystem.CodeInvalidArgument, `no file in field "file"`},
		{"too many files", func() *httptest.ResponseRecorder {
			return e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "a"}, part{field: recordshttp.FormField, filename: "b.txt", body: "b"})
		}, http.StatusBadRequest, filesystem.CodeInvalidArgument, "more than 1 files"},
		{"file too large", func() *httptest.ResponseRecorder {
			return e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: strings.Repeat("a", 2<<10)})
		}, http.StatusRequestEntityTooLarge, filesystem.CodeTooLarge, ""},
		{"request too large", func() *httptest.ResponseRecorder {
			return e.upload(t, "p1", part{field: "padding", body: strings.Repeat("a", 128<<10)}, part{field: recordshttp.FormField, filename: "a.txt", body: "a"})
		}, http.StatusRequestEntityTooLarge, filesystem.CodeTooLarge, "request body too large"},
		{"malformed body", func() *httptest.ResponseRecorder {
			return e.do(http.MethodPost, "/owners/post/p1/gallery", strings.NewReader("--b\r\nbroken"), "multipart/form-data; boundary=b")
		}, http.StatusBadRequest, filesystem.CodeInvalidArgument, ""},
		{"unknown owner type", func() *httptest.ResponseRecorder {
			body, ct := form(t, part{field: recordshttp.FormField, filename: "a.txt", body: "a"})
			return e.do(http.MethodPost, "/owners/user/u1/avatar", body, ct)
		}, http.StatusBadRequest, filesystem.CodeInvalidArgument, "not registered"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantError(t, c.rec(), c.status, c.code, c.msg)
			if n := e.objects(t); n != 0 {
				t.Fatalf("%d objects left", n)
			}
		})
	}
}

func TestRefusedByHooks(t *testing.T) {
	e := newEnv(t)
	rec := e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "a"})
	wantStatus(t, rec, http.StatusCreated)
	af := decode[recordshttp.ListResponse](t, rec).Files[0]
	routes := map[recordshttp.Action]func() *httptest.ResponseRecorder{
		recordshttp.ActionUpload: func() *httptest.ResponseRecorder {
			return e.upload(t, "p1", part{field: recordshttp.FormField, filename: "b.txt", body: "b"})
		},
		recordshttp.ActionList: func() *httptest.ResponseRecorder { return e.do(http.MethodGet, "/owners/post/p1/gallery", nil, "") },
		recordshttp.ActionReorder: func() *httptest.ResponseRecorder {
			return e.do(http.MethodPut, "/owners/post/p1/gallery/order", strings.NewReader(`{"ids":[]}`), "application/json")
		},
		recordshttp.ActionDetach: func() *httptest.ResponseRecorder {
			return e.do(http.MethodDelete, "/attachments/"+af.AttachmentID, nil, "")
		},
		recordshttp.ActionRead:  func() *httptest.ResponseRecorder { return e.do(http.MethodGet, "/files/"+af.FileID, nil, "") },
		recordshttp.ActionTrash: func() *httptest.ResponseRecorder { return e.do(http.MethodDelete, "/files/"+af.FileID, nil, "") },
		recordshttp.ActionReplace: func() *httptest.ResponseRecorder {
			body, ct := form(t, part{field: recordshttp.FormField, filename: "b.txt", body: "b"})
			return e.do(http.MethodPut, "/files/"+af.FileID, body, ct)
		},
		recordshttp.ActionRestore: func() *httptest.ResponseRecorder {
			return e.do(http.MethodPost, "/files/"+af.FileID+"/restore", nil, "")
		},
		recordshttp.ActionPurge: func() *httptest.ResponseRecorder {
			return e.do(http.MethodPost, "/files/"+af.FileID+"/purge", nil, "")
		},
		recordshttp.ActionSetVisibility: func() *httptest.ResponseRecorder {
			return e.do(http.MethodPut, "/files/"+af.FileID+"/visibility", strings.NewReader(`{"visibility":"public"}`), "application/json")
		},
		recordshttp.ActionSetAttachment: func() *httptest.ResponseRecorder {
			return e.do(http.MethodPut, "/owners/post/p2/gallery", strings.NewReader(`{"file_id":"`+af.FileID+`"}`), "application/json")
		},
		recordshttp.ActionSetText: func() *httptest.ResponseRecorder {
			return e.do(http.MethodPut, "/files/"+af.FileID+"/text", strings.NewReader(`{"alt":"a"}`), "application/json")
		},
	}
	if len(routes) != len(recordshttp.ActionValues) {
		t.Fatalf("%d routes for %d actions", len(routes), len(recordshttp.ActionValues))
	}
	for _, action := range recordshttp.ActionValues {
		t.Run(string(action), func(t *testing.T) {
			// The bodies carry only the client-visible message, like every
			// other error, never the "filesystem: permission:" prefix.
			e.anon, e.deny = true, ""
			if er := decode[recordshttp.ErrorResponse](t, routes[action]()); er.Error != "not signed in" || er.Code != filesystem.CodePermission {
				t.Fatalf("unauthenticated = %+v", er)
			}
			wantError(t, routes[action](), http.StatusUnauthorized, filesystem.CodePermission, "not signed in")
			e.anon, e.deny = false, action
			if er := decode[recordshttp.ErrorResponse](t, routes[action]()); er.Error != "forbidden" || er.Code != filesystem.CodePermission {
				t.Fatalf("forbidden = %+v", er)
			}
			wantError(t, routes[action](), http.StatusForbidden, filesystem.CodePermission, "forbidden")
		})
	}
	e.deny = ""
	if got := e.list(t, "p1"); len(got) != 1 || got[0].FileID != af.FileID {
		t.Fatalf("a refused request changed the files: %+v", got)
	}
	if f, err := e.store.File(t.Context(), tenant, af.FileID); err != nil || f.Visibility != filesystem.VisibilityPrivate {
		t.Fatalf("a refused request changed the file: %+v, %v", f, err)
	}
	if got := e.list(t, "p2"); len(got) != 0 {
		t.Fatalf("a refused set-attachment attached: %+v", got)
	}
}

func TestReorder(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		wantStatus(t, e.upload(t, "p1", part{field: recordshttp.FormField, filename: name, body: name}), http.StatusCreated)
	}
	files := e.list(t, "p1")
	ids := []string{files[2].AttachmentID, files[0].AttachmentID, files[1].AttachmentID}
	body, _ := json.Marshal(recordshttp.ReorderRequest{IDs: ids})
	wantStatus(t, e.do(http.MethodPut, "/owners/post/p1/gallery/order", bytes.NewReader(body), "application/json"), http.StatusNoContent)
	got := e.list(t, "p1")
	if got[0].Filename != "c.txt" || got[1].Filename != "a.txt" || got[2].Filename != "b.txt" {
		t.Fatalf("order = %s, %s, %s", got[0].Filename, got[1].Filename, got[2].Filename)
	}

	for name, c := range map[string]struct {
		body string
		msg  string
	}{
		"not JSON":      {"ids", "invalid JSON body"},
		"unknown field": {`{"ids":[],"extra":1}`, "invalid JSON body"},
		"stale list":    {`{"ids":["` + ids[0] + `"]}`, "exactly once"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.do(http.MethodPut, "/owners/post/p1/gallery/order", strings.NewReader(c.body), "application/json")
			wantError(t, rec, http.StatusBadRequest, filesystem.CodeInvalidArgument, c.msg)
		})
	}
}

func TestDetach(t *testing.T) {
	e := newEnv(t)
	rec := e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "a"})
	af := decode[recordshttp.ListResponse](t, rec).Files[0]

	wantStatus(t, e.do(http.MethodDelete, "/attachments/"+af.AttachmentID, nil, ""), http.StatusNoContent)
	last := e.calls[len(e.calls)-1]
	want := call{recordshttp.ActionDetach, recordshttp.Target{Scope: tenant, Owner: records.Owner{Type: "post", ID: "p1"}, Role: "gallery", FileID: af.FileID}}
	if last != want {
		t.Fatalf("Authorize got %+v, want %+v", last, want)
	}
	if len(e.list(t, "p1")) != 0 {
		t.Fatal("still attached")
	}
	if _, err := e.store.File(t.Context(), tenant, af.FileID); err != nil {
		t.Fatalf("detach removed the file: %v", err)
	}
	calls := len(e.calls)
	wantError(t, e.do(http.MethodDelete, "/attachments/"+af.AttachmentID, nil, ""), http.StatusNotFound, filesystem.CodeNotFound, "Not Found")
	if len(e.calls) != calls {
		t.Fatal("Authorize ran for an attachment that does not exist")
	}
}

func TestReadAndTrash(t *testing.T) {
	e := newEnv(t, func(c *recordshttp.Config, _ *[]filesystem.Option) { c.LinkLifetime = time.Minute })
	rec := e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "a"})
	af := decode[recordshttp.ListResponse](t, rec).Files[0]

	rec = e.do(http.MethodGet, "/files/"+af.FileID, nil, "")
	wantStatus(t, rec, http.StatusFound)
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "https://cdn.test/media/") || rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("redirect = %q, headers %v", loc, rec.Header())
	}
	if e.expires != time.Minute {
		t.Fatalf("link lifetime = %v", e.expires)
	}
	if tg := e.calls[len(e.calls)-1].target; tg.Scope != tenant || tg.FileID != af.FileID || tg.Owner != (records.Owner{}) || tg.File == nil || tg.File.ID != af.FileID {
		t.Fatalf("Authorize target = %+v", tg)
	}

	wantStatus(t, e.do(http.MethodDelete, "/files/"+af.FileID, nil, ""), http.StatusNoContent)
	wantError(t, e.do(http.MethodGet, "/files/"+af.FileID, nil, ""), http.StatusNotFound, filesystem.CodeNotFound, "")
	if len(e.list(t, "p1")) != 0 {
		t.Fatal("a trashed file is still listed")
	}
	wantError(t, e.do(http.MethodGet, "/files/fil_missing", nil, ""), http.StatusNotFound, filesystem.CodeNotFound, "")
	wantError(t, e.do(http.MethodDelete, "/files/fil_missing", nil, ""), http.StatusNotFound, filesystem.CodeNotFound, "")
}

func TestNoLinks(t *testing.T) {
	e := newEnv(t, noSigner)
	rec := e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "a"})
	af := decode[recordshttp.ListResponse](t, rec).Files[0]
	wantError(t, e.do(http.MethodGet, "/owners/post/p1/gallery", nil, ""), http.StatusNotImplemented, filesystem.CodeUnsupported, "Not Implemented")
	wantError(t, e.do(http.MethodGet, "/files/"+af.FileID, nil, ""), http.StatusNotImplemented, filesystem.CodeUnsupported, "Not Implemented")
}

// TestDatabaseDown closes the database inside Authorize, after the scope
// and permission checks passed: every route reports a bare 500 without
// the driver's message.
func TestDatabaseDown(t *testing.T) {
	routes := map[string]func(e *env, af recordshttp.AttachedFile) *httptest.ResponseRecorder{
		"list": func(e *env, _ recordshttp.AttachedFile) *httptest.ResponseRecorder {
			return e.do(http.MethodGet, "/owners/post/p1/gallery", nil, "")
		},
		"reorder": func(e *env, af recordshttp.AttachedFile) *httptest.ResponseRecorder {
			return e.do(http.MethodPut, "/owners/post/p1/gallery/order", strings.NewReader(`{"ids":["`+af.AttachmentID+`"]}`), "application/json")
		},
		"detach": func(e *env, af recordshttp.AttachedFile) *httptest.ResponseRecorder {
			return e.do(http.MethodDelete, "/attachments/"+af.AttachmentID, nil, "")
		},
		"read": func(e *env, af recordshttp.AttachedFile) *httptest.ResponseRecorder {
			// The read route queries nothing after Authorize: the database
			// fails at the file's load instead.
			e.onAuth, e.onScope = nil, func() { _ = e.db.Close() }
			return e.do(http.MethodGet, "/files/"+af.FileID, nil, "")
		},
		"restore": func(e *env, af recordshttp.AttachedFile) *httptest.ResponseRecorder {
			return e.do(http.MethodPost, "/files/"+af.FileID+"/restore", nil, "")
		},
		"purge": func(e *env, af recordshttp.AttachedFile) *httptest.ResponseRecorder {
			return e.do(http.MethodPost, "/files/"+af.FileID+"/purge", nil, "")
		},
		"set visibility": func(e *env, af recordshttp.AttachedFile) *httptest.ResponseRecorder {
			return e.do(http.MethodPut, "/files/"+af.FileID+"/visibility", strings.NewReader(`{"visibility":"public"}`), "application/json")
		},
		"set attachment": func(e *env, af recordshttp.AttachedFile) *httptest.ResponseRecorder {
			return e.do(http.MethodPut, "/owners/post/p2/gallery", strings.NewReader(`{"file_id":"`+af.FileID+`"}`), "application/json")
		},
		"replace": func(e *env, af recordshttp.AttachedFile) *httptest.ResponseRecorder {
			body, ct := form(t, part{field: recordshttp.FormField, filename: "b.txt", body: "b"})
			return e.do(http.MethodPut, "/files/"+af.FileID, body, ct)
		},
		"trash": func(e *env, af recordshttp.AttachedFile) *httptest.ResponseRecorder {
			return e.do(http.MethodDelete, "/files/"+af.FileID, nil, "")
		},
	}
	for name, route := range routes {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			rec := e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "a"})
			af := decode[recordshttp.ListResponse](t, rec).Files[0]
			e.onAuth = func() { _ = e.db.Close() }
			wantError(t, route(e, af), http.StatusInternalServerError, filesystem.CodeUnknown, "Internal Server Error")
		})
	}
}

func TestReplace(t *testing.T) {
	e := newEnv(t)
	rec := e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "first"})
	af := decode[recordshttp.ListResponse](t, rec).Files[0]
	put := func(id string, parts ...part) *httptest.ResponseRecorder {
		body, ct := form(t, parts...)
		return e.do(http.MethodPut, "/files/"+id, body, ct)
	}

	rec = put(af.FileID, part{field: "caption", body: "skipped"}, part{field: recordshttp.FormField, filename: "b.txt", body: "second, longer"})
	wantStatus(t, rec, http.StatusOK)
	got := decode[recordshttp.FileResponse](t, rec)
	if got.FileID != af.FileID || got.Filename != "b.txt" || got.Size != int64(len("second, longer")) {
		t.Fatalf("replaced = %+v", got)
	}
	listed := e.list(t, "p1")
	if len(listed) != 1 || listed[0].AttachmentID != af.AttachmentID || listed[0].Size != got.Size {
		t.Fatalf("listed after replace = %+v", listed)
	}
	if n := e.objects(t); n != 1 {
		t.Fatalf("%d objects after replace, want 1", n)
	}

	cases := []struct {
		name   string
		rec    func() *httptest.ResponseRecorder
		status int
		code   filesystem.Code
	}{
		{"not multipart", func() *httptest.ResponseRecorder {
			return e.do(http.MethodPut, "/files/"+af.FileID, strings.NewReader("x"), "text/plain")
		}, http.StatusBadRequest, filesystem.CodeInvalidArgument},
		{"no file", func() *httptest.ResponseRecorder { return put(af.FileID, part{field: "caption", body: "x"}) }, http.StatusBadRequest, filesystem.CodeInvalidArgument},
		{"malformed", func() *httptest.ResponseRecorder {
			return e.do(http.MethodPut, "/files/"+af.FileID, strings.NewReader("--b\r\nbroken"), "multipart/form-data; boundary=b")
		}, http.StatusBadRequest, filesystem.CodeInvalidArgument},
		{"refused type", func() *httptest.ResponseRecorder {
			return put(af.FileID, part{field: recordshttp.FormField, filename: "x.html", body: "<!DOCTYPE html><p>x</p>"})
		}, http.StatusUnsupportedMediaType, filesystem.CodeRejected},
		{"too large", func() *httptest.ResponseRecorder {
			return put(af.FileID, part{field: recordshttp.FormField, filename: "x.txt", body: strings.Repeat("x", 2<<10)})
		}, http.StatusRequestEntityTooLarge, filesystem.CodeTooLarge},
		{"missing file", func() *httptest.ResponseRecorder {
			return put("fil_missing", part{field: recordshttp.FormField, filename: "x.txt", body: "x"})
		}, http.StatusNotFound, filesystem.CodeNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantError(t, c.rec(), c.status, c.code, "")
			if n := e.objects(t); n != 1 {
				t.Fatalf("%d objects", n)
			}
		})
	}
}

// TestFileRoutesLoadFirst pins that every route naming a file loads it
// before Authorize: the hook sees the file, and a file of another scope
// is 404 without the hook ever running.
func TestFileRoutesLoadFirst(t *testing.T) {
	e := newEnv(t)
	rec := e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "a"})
	af := decode[recordshttp.ListResponse](t, rec).Files[0]
	routes := map[recordshttp.Action]func() *httptest.ResponseRecorder{
		recordshttp.ActionRead: func() *httptest.ResponseRecorder { return e.do(http.MethodGet, "/files/"+af.FileID, nil, "") },
		recordshttp.ActionReplace: func() *httptest.ResponseRecorder {
			body, ct := form(t, part{field: recordshttp.FormField, filename: "b.txt", body: "b"})
			return e.do(http.MethodPut, "/files/"+af.FileID, body, ct)
		},
		recordshttp.ActionTrash: func() *httptest.ResponseRecorder { return e.do(http.MethodDelete, "/files/"+af.FileID, nil, "") },
		recordshttp.ActionRestore: func() *httptest.ResponseRecorder {
			return e.do(http.MethodPost, "/files/"+af.FileID+"/restore", nil, "")
		},
		recordshttp.ActionPurge: func() *httptest.ResponseRecorder { return e.do(http.MethodPost, "/files/"+af.FileID+"/purge", nil, "") },
		recordshttp.ActionSetVisibility: func() *httptest.ResponseRecorder {
			return e.do(http.MethodPut, "/files/"+af.FileID+"/visibility", strings.NewReader(`{"visibility":"public"}`), "application/json")
		},
		recordshttp.ActionSetAttachment: func() *httptest.ResponseRecorder {
			return e.do(http.MethodPut, "/owners/post/p2/gallery", strings.NewReader(`{"file_id":"`+af.FileID+`"}`), "application/json")
		},
		recordshttp.ActionSetText: func() *httptest.ResponseRecorder {
			return e.do(http.MethodPut, "/files/"+af.FileID+"/text", strings.NewReader(`{"alt":"a"}`), "application/json")
		},
	}
	for action, route := range routes {
		t.Run(string(action), func(t *testing.T) {
			e.tenant, e.deny = "t2", ""
			calls := len(e.calls)
			wantError(t, route(), http.StatusNotFound, filesystem.CodeNotFound, "Not Found")
			if len(e.calls) != calls {
				t.Fatalf("Authorize ran for a file of another scope: %+v", e.calls[calls:])
			}
			e.tenant, e.deny = "", action
			wantError(t, route(), http.StatusForbidden, filesystem.CodePermission, "forbidden")
			got := e.calls[calls]
			if got.action != action || got.target.File == nil || got.target.File.ID != af.FileID || got.target.File.UploadedBy != "user:t" || got.target.FileID != af.FileID || got.target.Scope != tenant {
				t.Fatalf("Authorize got %+v (file %+v)", got, got.target.File)
			}
		})
	}
}

func TestRestoreAndPurge(t *testing.T) {
	e := newEnv(t)
	rec := e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "a"})
	af := decode[recordshttp.ListResponse](t, rec).Files[0]

	wantStatus(t, e.do(http.MethodDelete, "/files/"+af.FileID, nil, ""), http.StatusNoContent)
	// Authorize sees that the file is in the trash, so a rule can allow
	// restore only for trashed files.
	wantStatus(t, e.do(http.MethodPost, "/files/"+af.FileID+"/restore", nil, ""), http.StatusNoContent)
	if last := e.calls[len(e.calls)-1]; last.action != recordshttp.ActionRestore || last.target.File.DeletedAt == nil {
		t.Fatalf("restore authorized with %+v", last.target.File)
	}
	if got := e.list(t, "p1"); len(got) != 1 || got[0].FileID != af.FileID {
		t.Fatalf("listed after restore = %+v", got)
	}
	wantStatus(t, e.do(http.MethodGet, "/files/"+af.FileID, nil, ""), http.StatusFound)
	// Restoring a live file does nothing.
	wantStatus(t, e.do(http.MethodPost, "/files/"+af.FileID+"/restore", nil, ""), http.StatusNoContent)

	wantStatus(t, e.do(http.MethodPost, "/files/"+af.FileID+"/purge", nil, ""), http.StatusNoContent)
	if _, err := e.store.File(t.Context(), tenant, af.FileID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("purged file still loads: %v", err)
	}
	if n := e.objects(t); n != 0 {
		t.Fatalf("%d objects after purge", n)
	}
	if len(e.list(t, "p1")) != 0 {
		t.Fatal("purged file still attached")
	}
	wantError(t, e.do(http.MethodPost, "/files/"+af.FileID+"/purge", nil, ""), http.StatusNotFound, filesystem.CodeNotFound, "")
	wantError(t, e.do(http.MethodPost, "/files/"+af.FileID+"/restore", nil, ""), http.StatusNotFound, filesystem.CodeNotFound, "")
}

func TestSetVisibility(t *testing.T) {
	e := newEnv(t)
	rec := e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "a"})
	af := decode[recordshttp.ListResponse](t, rec).Files[0]
	put := func(body string) *httptest.ResponseRecorder {
		return e.do(http.MethodPut, "/files/"+af.FileID+"/visibility", strings.NewReader(body), "application/json")
	}

	wantStatus(t, put(`{"visibility":"public"}`), http.StatusNoContent)
	f, err := e.store.File(t.Context(), tenant, af.FileID)
	if err != nil || f.Visibility != filesystem.VisibilityPublic {
		t.Fatalf("after set = %+v, %v", f, err)
	}
	if st, err := e.disk.Stat(t.Context(), f.Key); err != nil || st.Visibility != filesystem.VisibilityPublic {
		t.Fatalf("object = %+v, %v", st, err)
	}
	for name, c := range map[string]struct{ body, msg string }{
		"not JSON":         {"public", "invalid JSON body"},
		"unknown field":    {`{"visibility":"public","x":1}`, "invalid JSON body"},
		"unknown value":    {`{"visibility":"secret"}`, "unknown visibility"},
		"missing value":    {`{}`, "unknown visibility"},
		"trailing garbage": {`{"visibility":`, "invalid JSON body"},
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, put(c.body), http.StatusBadRequest, filesystem.CodeInvalidArgument, c.msg)
		})
	}
	if f, _ := e.store.File(t.Context(), tenant, af.FileID); f.Visibility != filesystem.VisibilityPublic {
		t.Fatalf("a refused change changed the file: %+v", f)
	}
}

func TestSetAttachment(t *testing.T) {
	e := newEnv(t)
	rec := e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "a"}, part{field: recordshttp.FormField, filename: "b.txt", body: "b"})
	files := decode[recordshttp.ListResponse](t, rec).Files
	set := func(owner, body string) *httptest.ResponseRecorder {
		return e.do(http.MethodPut, "/owners/post/"+owner+"/gallery", strings.NewReader(body), "application/json")
	}

	// The default keeps the files it detaches; another owner may share one.
	rec = set("p2", `{"file_id":"`+files[0].FileID+`"}`)
	wantStatus(t, rec, http.StatusOK)
	got := decode[recordshttp.AttachedFile](t, rec)
	if got.FileID != files[0].FileID || got.Filename != "a.txt" || got.Position != 1 || got.AttachmentID == "" {
		t.Fatalf("set = %+v", got)
	}
	owner := records.Owner{Type: "post", ID: "p2"}
	n := len(e.calls)
	if e.calls[n-2].action != recordshttp.ActionSetAttachment || e.calls[n-2].target.Owner != owner || e.calls[n-2].target.Role != "gallery" || e.calls[n-2].target.File.ID != files[0].FileID {
		t.Fatalf("set authorized with %+v", e.calls[n-2])
	}
	// Attaching exposes the file through the owner's list, so the request
	// must also be allowed to read it, asked with the file alone.
	if e.calls[n-1].action != recordshttp.ActionRead || e.calls[n-1].target.Owner != (records.Owner{}) || e.calls[n-1].target.File.ID != files[0].FileID {
		t.Fatalf("read authorized with %+v", e.calls[n-1])
	}

	// Replacing p1's two files with b: a is shared with p2, so only
	// detached; b is the new one.
	wantStatus(t, set("p1", `{"file_id":"`+files[1].FileID+`","previous":"trash"}`), http.StatusOK)
	if l := e.list(t, "p1"); len(l) != 1 || l[0].FileID != files[1].FileID {
		t.Fatalf("p1 after set = %+v", l)
	}
	if f, _ := e.store.File(t.Context(), tenant, files[0].FileID); f.DeletedAt != nil {
		t.Fatal("a file another owner uses was trashed")
	}
	// p2 swaps a for b and trashes a, which nothing else uses now.
	wantStatus(t, set("p2", `{"file_id":"`+files[1].FileID+`","previous":"trash"}`), http.StatusOK)
	if f, _ := e.store.File(t.Context(), tenant, files[0].FileID); f.DeletedAt == nil {
		t.Fatal("previous trash left the old file live")
	}

	e.deny = recordshttp.ActionRead
	wantError(t, set("p3", `{"file_id":"`+files[1].FileID+`"}`), http.StatusForbidden, filesystem.CodePermission, "forbidden")
	if len(e.list(t, "p3")) != 0 {
		t.Fatal("attached a file the request may not read")
	}
	e.deny = ""

	for name, c := range map[string]struct {
		body   string
		status int
		code   filesystem.Code
		msg    string
	}{
		"not JSON":         {"x", http.StatusBadRequest, filesystem.CodeInvalidArgument, "invalid JSON body"},
		"unknown field":    {`{"file_id":"x","y":1}`, http.StatusBadRequest, filesystem.CodeInvalidArgument, "invalid JSON body"},
		"no file":          {`{}`, http.StatusBadRequest, filesystem.CodeInvalidArgument, "file_id is required"},
		"unknown file":     {`{"file_id":"fil_missing"}`, http.StatusNotFound, filesystem.CodeNotFound, ""},
		"trashed file":     {`{"file_id":"` + files[0].FileID + `"}`, http.StatusNotFound, filesystem.CodeNotFound, ""},
		"unknown previous": {`{"file_id":"` + files[1].FileID + `","previous":"shred"}`, http.StatusBadRequest, filesystem.CodeInvalidArgument, "unknown Previous"},
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, set("p4", c.body), c.status, c.code, c.msg)
		})
	}
	if len(e.list(t, "p4")) != 0 {
		t.Fatal("a refused set attached a file")
	}
}

func partial(c *recordshttp.Config, _ *[]filesystem.Option) { c.Partial, c.MaxFiles = true, 3 }

func TestPartialUpload(t *testing.T) {
	e := newEnv(t, partial)
	rec := e.upload(t, "p1",
		part{field: recordshttp.FormField, filename: "a.txt", body: "a"},
		part{field: recordshttp.FormField, filename: "../page.html", body: "<!DOCTYPE html><script>alert(1)</script>"},
		part{field: recordshttp.FormField, filename: "b.png", body: pngSig + "pixels"},
	)
	wantStatus(t, rec, http.StatusOK)
	got := decode[recordshttp.ListResponse](t, rec)
	if len(got.Files) != 2 || got.Files[0].Filename != "a.txt" || got.Files[1].Filename != "b.png" {
		t.Fatalf("stored = %+v", got.Files)
	}
	if len(got.Failed) != 1 || got.Failed[0].Filename != "page.html" || got.Failed[0].Code != filesystem.CodeRejected || !strings.Contains(got.Failed[0].Error, "text/html") {
		t.Fatalf("failed = %+v", got.Failed)
	}
	if l := e.list(t, "p1"); len(l) != 2 || e.objects(t) != 2 {
		t.Fatalf("listed %d, %d objects", len(l), e.objects(t))
	}

	rec = e.upload(t, "p2", part{field: recordshttp.FormField, filename: "c.txt", body: "c"})
	wantStatus(t, rec, http.StatusCreated)
	if got := decode[recordshttp.ListResponse](t, rec); len(got.Failed) != 0 || strings.Contains(rec.Body.String(), `"failed"`) {
		t.Fatalf("a full success reports failures: %s", rec.Body.String())
	}

	// Nothing stored: the first failure's status, with every failure.
	rec = e.upload(t, "p3",
		part{field: recordshttp.FormField, filename: "big.txt", body: strings.Repeat("a", 2<<10)},
		part{field: recordshttp.FormField, filename: "x.html", body: "<!DOCTYPE html><p>x</p>"},
	)
	wantStatus(t, rec, http.StatusRequestEntityTooLarge)
	got = decode[recordshttp.ListResponse](t, rec)
	if got.Files == nil || len(got.Files) != 0 || len(got.Failed) != 2 || got.Failed[0].Code != filesystem.CodeTooLarge || got.Failed[1].Code != filesystem.CodeRejected {
		t.Fatalf("all failed = %s", rec.Body.String())
	}

	// Request-level failures purge what the request attached.
	before := e.objects(t)
	cases := []struct {
		name   string
		parts  []part
		status int
		code   filesystem.Code
	}{
		{"too many files", []part{
			{field: recordshttp.FormField, filename: "1.txt", body: "1"},
			{field: recordshttp.FormField, filename: "2.txt", body: "2"},
			{field: recordshttp.FormField, filename: "3.txt", body: "3"},
			{field: recordshttp.FormField, filename: "4.txt", body: "4"},
		}, http.StatusBadRequest, filesystem.CodeInvalidArgument},
		{"request too large", []part{
			{field: recordshttp.FormField, filename: "1.txt", body: "1"},
			{field: "padding", body: strings.Repeat("a", 128<<10)},
		}, http.StatusRequestEntityTooLarge, filesystem.CodeTooLarge},
		{"no file", []part{{field: "caption", body: "x"}}, http.StatusBadRequest, filesystem.CodeInvalidArgument},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantError(t, e.upload(t, "p4", c.parts...), c.status, c.code, "")
			if l := e.list(t, "p4"); len(l) != 0 {
				t.Fatalf("a failed request left %d files attached", len(l))
			}
			if n := e.objects(t); n != before {
				t.Fatalf("%d objects, want %d", n, before)
			}
		})
	}
}

// TestTooLargeHidesKey trips the request cap while a file is being
// written (the padding passes NextPart, the file pushes the body over):
// the answer is 413 without the Disk's annotated "write <disk>:<key>"
// text.
func TestTooLargeHidesKey(t *testing.T) {
	for name, opt := range map[string]option{
		"all or none": func(c *recordshttp.Config, _ *[]filesystem.Option) { c.MaxFiles = 1 },
		"partial":     func(c *recordshttp.Config, _ *[]filesystem.Option) { c.MaxFiles, c.Partial = 1, true },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, opt)
			rec := e.upload(t, "p1", part{field: "padding", body: strings.Repeat("p", 64<<10-200)}, part{field: recordshttp.FormField, filename: "a.txt", body: strings.Repeat("a", 900)})
			// The cap's own words, not just the status text.
			wantError(t, rec, http.StatusRequestEntityTooLarge, filesystem.CodeTooLarge, "request body too large")
			if er := decode[recordshttp.ErrorResponse](t, rec); strings.Contains(er.Error, "media") || strings.Contains(er.Error, "write") {
				t.Fatalf("message leaks the write: %q", er.Error)
			}
			if n := e.objects(t); n != 0 {
				t.Fatalf("%d objects left", n)
			}
		})
	}
}

// TestRetryAfter passes a backend's requested wait on to the client.
func TestRetryAfter(t *testing.T) {
	busy := func(_ *recordshttp.Config, o *[]filesystem.Option) {
		*o = append(*o, filesystem.WithInterceptors(func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
			if op.Kind == filesystem.OpWrite {
				return filesystem.Errorf(filesystem.CodeUnavailable, "slow down").WithRetryAfter(1500 * time.Millisecond)
			}
			return call(ctx)
		}))
	}
	e := newEnv(t, busy)
	rec := e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "a"})
	wantError(t, rec, http.StatusServiceUnavailable, filesystem.CodeUnavailable, "")
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want 2", got)
	}
}

// TestNestedErrorHidesKey: a client-visible error wrapping an annotated
// *filesystem.Error shows only what the inner error's own code allows,
// never its key.
func TestNestedErrorHidesKey(t *testing.T) {
	inner := &filesystem.Error{Code: filesystem.CodeNotFound, Op: filesystem.OpRead, Disk: "media", Key: "secret/key.png", Err: errors.New("gone")}
	failWrite := func(_ *recordshttp.Config, o *[]filesystem.Option) {
		*o = append(*o, filesystem.WithInterceptors(func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
			if op.Kind == filesystem.OpWrite {
				return filesystem.NewError(filesystem.CodeRejected, inner)
			}
			return call(ctx)
		}))
	}
	e := newEnv(t, failWrite)
	rec := e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "a"})
	wantStatus(t, rec, http.StatusUnsupportedMediaType)
	if er := decode[recordshttp.ErrorResponse](t, rec); strings.Contains(er.Error, "secret") || er.Error != http.StatusText(http.StatusUnsupportedMediaType) {
		t.Fatalf("message = %q", er.Error)
	}
}

func TestSetText(t *testing.T) {
	e := newEnv(t)
	rec := e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "a"})
	af := decode[recordshttp.ListResponse](t, rec).Files[0]
	if af.Alt != "" || af.Caption != "" || af.Title != "" {
		t.Fatalf("a new file has text: %+v", af)
	}
	put := func(body string) *httptest.ResponseRecorder {
		return e.do(http.MethodPut, "/files/"+af.FileID+"/text", strings.NewReader(body), "application/json")
	}
	wantStatus(t, put(`{"alt":"A dog","caption":"Day one.\nLow tide.","title":"Beach"}`), http.StatusNoContent)
	list := decode[recordshttp.ListResponse](t, e.do(http.MethodGet, "/owners/post/p1/gallery", nil, ""))
	if got := list.Files[0]; got.Alt != "A dog" || got.Caption != "Day one.\nLow tide." || got.Title != "Beach" {
		t.Fatalf("listed = %+v", got)
	}
	// An absent field removes it.
	wantStatus(t, put(`{"alt":"A dog"}`), http.StatusNoContent)
	if f, _ := e.store.File(t.Context(), tenant, af.FileID); f.Text() != (records.Text{Alt: "A dog"}) {
		t.Fatalf("after a partial body = %+v", f.Text())
	}
	for name, c := range map[string]struct{ body, msg string }{
		"not JSON":      {"alt", "invalid JSON body"},
		"unknown field": {`{"alt":"a","x":1}`, "invalid JSON body"},
		"control char":  {`{"title":"a\tb"}`, "title must be"},
		"too long":      {`{"alt":"` + strings.Repeat("a", records.MaxAltChars+1) + `"}`, "alt must be"},
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, put(c.body), http.StatusBadRequest, filesystem.CodeInvalidArgument, c.msg)
		})
	}
}

// TestPartialRetryAfter: a per-file failure that asks for a wait puts it
// on the answer, whether some files were attached or none.
func TestPartialRetryAfter(t *testing.T) {
	busy := func(c *recordshttp.Config, o *[]filesystem.Option) {
		partial(c, o)
		*o = append(*o, filesystem.WithInterceptors(func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
			if op.Kind == filesystem.OpWrite && strings.HasSuffix(op.Key, ".png") {
				return filesystem.Errorf(filesystem.CodeUnavailable, "slow down").WithRetryAfter(3 * time.Second)
			}
			return call(ctx)
		}))
	}
	e := newEnv(t, busy)
	rec := e.upload(t, "p1",
		part{field: recordshttp.FormField, filename: "a.txt", body: "a"},
		part{field: recordshttp.FormField, filename: "b.png", body: pngSig + "pixels"},
	)
	wantStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After = %q, want 3", got)
	}
	rec = e.upload(t, "p2", part{field: recordshttp.FormField, filename: "c.png", body: pngSig + "pixels"})
	wantStatus(t, rec, http.StatusServiceUnavailable)
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("all failed: Retry-After = %q, want 3", got)
	}
	rec = e.upload(t, "p3", part{field: recordshttp.FormField, filename: "d.txt", body: "d"})
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("a full success has Retry-After %q", got)
	}
}

// TestPurgeRefusesAChangedTrashState restores the file between the
// handler's load (which Authorize saw as trashed) and the purge: the
// purge is refused and the file kept.
func TestPurgeRefusesAChangedTrashState(t *testing.T) {
	e := newEnv(t)
	rec := e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "a"})
	af := decode[recordshttp.ListResponse](t, rec).Files[0]
	if err := e.store.Trash(t.Context(), tenant, af.FileID); err != nil {
		t.Fatal(err)
	}
	e.onAuth = func() { _ = e.store.Restore(context.Background(), tenant, af.FileID) }
	rec = e.do(http.MethodPost, "/files/"+af.FileID+"/purge", nil, "")
	e.onAuth = nil
	wantError(t, rec, http.StatusServiceUnavailable, filesystem.CodeUnavailable, "")
	if f, err := e.store.File(t.Context(), tenant, af.FileID); err != nil || f.DeletedAt != nil {
		t.Fatalf("file after the refused purge = %+v, %v", f, err)
	}
}

// watchedBody is a request body that records whether it was read.
type watchedBody struct {
	io.Reader
	read *bool
}

func (b watchedBody) Read(p []byte) (int, error) {
	*b.read = true
	return b.Reader.Read(p)
}

// headerPlan is the request header the Resolve tests choose a plan by.
const headerPlan = "X-Plan"

// planResolver lowers MaxSize for the "free" plan, refuses the "busy" one
// as a rate limit, and returns an invalid policy for "broken".
func planResolver(r *http.Request, base upload.Policy) (upload.Policy, error) {
	switch r.Header.Get(headerPlan) {
	case "free":
		base.MaxSize = 4
	case "busy":
		return upload.Policy{}, filesystem.Errorf(filesystem.CodeUnavailable, "slow down").WithRetryAfter(2 * time.Second)
	case "broken":
		base.MaxSize = -1
	}
	return base, nil
}

func TestResolve(t *testing.T) {
	e := newEnv(t, func(c *recordshttp.Config, _ *[]filesystem.Option) { c.Resolve = planResolver })
	send := func(plan, path, method string, read *bool) *httptest.ResponseRecorder {
		body, ct := form(t, part{field: recordshttp.FormField, filename: "a.txt", body: "hello"})
		req := httptest.NewRequest(method, path, watchedBody{body, read})
		req.Header.Set("Content-Type", ct)
		req.Header.Set(headerPlan, plan)
		rec := httptest.NewRecorder()
		e.srv.ServeHTTP(rec, req)
		return rec
	}
	var read bool
	wantStatus(t, send("paid", "/owners/post/p1/gallery", http.MethodPost, &read), http.StatusCreated)
	wantError(t, send("free", "/owners/post/p1/gallery", http.MethodPost, &read), http.StatusRequestEntityTooLarge, filesystem.CodeTooLarge, "")

	// A refusal answers before any byte of the body is read.
	for _, path := range []string{"/owners/post/p1/gallery", "/files/" + e.list(t, "p1")[0].FileID} {
		method := http.MethodPost
		if strings.HasPrefix(path, "/files/") {
			method = http.MethodPut
		}
		read = false
		rec := send("busy", path, method, &read)
		wantError(t, rec, http.StatusServiceUnavailable, filesystem.CodeUnavailable, "")
		if got := rec.Header().Get("Retry-After"); got != "2" || read {
			t.Fatalf("%s: Retry-After %q, body read %v", path, got, read)
		}
		read = false
		rec = send("broken", path, method, &read)
		wantError(t, rec, http.StatusInternalServerError, filesystem.CodeUnknown, http.StatusText(http.StatusInternalServerError))
		if strings.Contains(rec.Body.String(), "Resolve") || strings.Contains(rec.Body.String(), "MaxSize") {
			t.Fatalf("%s: the validation message reached the client: %s", path, rec.Body.String())
		}
		if read {
			t.Fatalf("%s: an invalid resolved policy read the body", path)
		}
	}
	// The replace route uses the resolved policy too.
	wantError(t, send("free", "/files/"+e.list(t, "p1")[0].FileID, http.MethodPut, &read), http.StatusRequestEntityTooLarge, filesystem.CodeTooLarge, "")
}

func TestMaxTotal(t *testing.T) {
	total := func(n int64, partialToo bool) option {
		return func(c *recordshttp.Config, _ *[]filesystem.Option) { c.MaxTotal, c.Partial = n, partialToo }
	}
	parts := []part{
		{field: recordshttp.FormField, filename: "a.txt", body: "aaa"},
		{field: recordshttp.FormField, filename: "b.txt", body: "bbb"},
		{field: recordshttp.FormField, filename: "c.txt", body: "ccc"},
	}
	// All or nothing: the third file passes the total, nothing is kept.
	e := newEnv(t, total(7, false))
	wantError(t, e.upload(t, "p1", parts...), http.StatusRequestEntityTooLarge, filesystem.CodeTooLarge, "in total")
	if n := e.objects(t); n != 0 || len(e.list(t, "p1")) != 0 {
		t.Fatalf("%d objects left", n)
	}
	wantStatus(t, e.upload(t, "p1", parts[:2]...), http.StatusCreated)

	// Partial: the files that fit are attached, the rest fail one by one,
	// whether a little room is left (7) or none (6).
	for _, n := range []int64{7, 6} {
		e := newEnv(t, total(n, true))
		rec := e.upload(t, "p1", parts...)
		wantStatus(t, rec, http.StatusOK)
		got := decode[recordshttp.ListResponse](t, rec)
		if len(got.Files) != 2 || len(got.Failed) != 1 || got.Failed[0].Code != filesystem.CodeTooLarge || !strings.Contains(got.Failed[0].Error, "in total") {
			t.Fatalf("MaxTotal %d: %s", n, rec.Body.String())
		}
	}

	// One file over the total on the replace route.
	e = newEnv(t, total(2, false))
	wantStatus(t, e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "a"}), http.StatusCreated)
	body, ct := form(t, part{field: recordshttp.FormField, filename: "b.txt", body: "bbb"})
	wantError(t, e.do(http.MethodPut, "/files/"+e.list(t, "p1")[0].FileID, body, ct), http.StatusRequestEntityTooLarge, filesystem.CodeTooLarge, "in total")

	if _, err := recordshttp.Handler(recordshttp.Config{Store: e.store, Disk: "media", Policy: upload.Policy{MaxSize: 1, Allowed: []string{"text/plain"}}, Scope: func(*http.Request) (string, error) { return "", nil },
		Authorize: func(*http.Request, recordshttp.Action, recordshttp.Target) error { return nil }, MaxTotal: -1}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || !strings.Contains(err.Error(), "MaxTotal") {
		t.Fatalf("negative MaxTotal = %v", err)
	}
}

func TestStatusRoute(t *testing.T) {
	e := newEnv(t)
	rec := e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "a"})
	af := decode[recordshttp.ListResponse](t, rec).Files[0]
	if af.Status != records.StatusReady {
		t.Fatalf("uploaded status = %q", af.Status)
	}
	get := func() *httptest.ResponseRecorder { return e.do(http.MethodGet, "/files/"+af.FileID+"/status", nil, "") }
	rec = get()
	wantStatus(t, rec, http.StatusOK)
	if got := decode[recordshttp.StatusResponse](t, rec); got.FileID != af.FileID || got.Status != records.StatusReady || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status = %+v, %q", got, rec.Header().Get("Cache-Control"))
	}
	if _, err := e.store.SetStatus(t.Context(), tenant, af.FileID, records.StatusFailed, "scan: infected"); err != nil {
		t.Fatal(err)
	}
	if got := decode[recordshttp.StatusResponse](t, get()); got.Status != records.StatusFailed || got.StatusDetail != "scan: infected" {
		t.Fatalf("failed status = %+v", got)
	}
	if got := e.list(t, "p1")[0]; got.Status != records.StatusFailed || got.StatusDetail != "scan: infected" {
		t.Fatalf("listed = %+v", got)
	}
	// Authorized as a read, and a trashed file is not found.
	e.deny = recordshttp.ActionRead
	wantError(t, get(), http.StatusForbidden, filesystem.CodePermission, "")
	e.deny = ""
	_ = e.store.Trash(t.Context(), tenant, af.FileID)
	wantError(t, get(), http.StatusNotFound, filesystem.CodeNotFound, "")
}
