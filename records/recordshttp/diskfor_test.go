package recordshttp_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/recordshttp"
	"github.com/ubgo/filesystem/upload"
)

// diskNames are the disks of a routing env: a fixed one, a tenant's own
// bucket, and a public and a private one for visibility routing.
var diskNames = []string{"media", "tenant-t1", "pub", "priv"}

// routeEnv is a handler over several disks, for the choice of an upload's
// disk (Config.Disk, DiskFor, WithVisibilityDisks).
type routeEnv struct {
	store *records.Store
	disks map[string]*filesystem.Disk
	srv   http.Handler
}

func newRouteEnv(t *testing.T, cfg recordshttp.Config, storeOpts ...records.Option) *routeEnv {
	t.Helper()
	e := &routeEnv{disks: map[string]*filesystem.Disk{}}
	var all []*filesystem.Disk
	var expires time.Duration
	for _, name := range diskNames {
		d, err := filesystem.NewDisk(name, memory.New(), filesystem.WithURLSigner(signer{&expires}))
		if err != nil {
			t.Fatal(err)
		}
		e.disks[name] = d
		all = append(all, d)
	}
	mgr, err := filesystem.NewManager(all...)
	if err != nil {
		t.Fatal(err)
	}
	db := testdb.Open(t)
	if e.store, err = records.New(db, testdb.Dialect(), mgr, append([]records.Option{records.WithOwnerTypes("post")}, storeOpts...)...); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	cfg.Store = e.store
	if cfg.Policy.MaxSize == 0 {
		cfg.Policy = upload.Policy{MaxSize: 1 << 10, Allowed: []string{"text/plain"}}
	}
	cfg.Scope = func(*http.Request) (string, error) { return tenant, nil }
	cfg.Authorize = func(*http.Request, recordshttp.Action, recordshttp.Target) error { return nil }
	if e.srv, err = recordshttp.Handler(cfg); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *routeEnv) upload(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := form(t, part{field: recordshttp.FormField, filename: "a.txt", body: "hello"})
	req := httptest.NewRequest(http.MethodPost, "/owners/post/p1/gallery", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	return rec
}

// counts returns the number of objects on every disk, by name.
func (e *routeEnv) counts(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	for name, d := range e.disks {
		page, err := d.ListPage(t.Context(), "", filesystem.ListOptions{Recursive: true})
		if err != nil {
			t.Fatal(err)
		}
		if n := len(page.Entries); n > 0 {
			out[name] = n
		}
	}
	return out
}

// uploadedDisk uploads one file, expects 201 and returns the disk its row
// names, checking the object is on that disk and nowhere else.
func (e *routeEnv) uploadedDisk(t *testing.T) string {
	t.Helper()
	rec := e.upload(t)
	wantStatus(t, rec, http.StatusCreated)
	got := decode[recordshttp.ListResponse](t, rec).Files
	f, err := e.store.File(t.Context(), tenant, got[0].FileID)
	if err != nil {
		t.Fatal(err)
	}
	if c := e.counts(t); len(c) != 1 || c[f.Disk] != 1 {
		t.Fatalf("objects = %v, row names disk %q", c, f.Disk)
	}
	return f.Disk
}

func TestDiskFixed(t *testing.T) {
	e := newRouteEnv(t, recordshttp.Config{Disk: "media"})
	if d := e.uploadedDisk(t); d != "media" {
		t.Fatalf("disk = %q, want media", d)
	}
}

func TestDiskFor(t *testing.T) {
	var got []recordshttp.Target
	e := newRouteEnv(t, recordshttp.Config{DiskFor: func(_ *http.Request, tg recordshttp.Target) (string, error) {
		got = append(got, tg)
		return "tenant-" + tg.Scope, nil
	}})
	if d := e.uploadedDisk(t); d != "tenant-t1" {
		t.Fatalf("disk = %q, want tenant-t1", d)
	}
	want := recordshttp.Target{Scope: tenant, Owner: records.Owner{Type: "post", ID: "p1"}, Role: "gallery"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("DiskFor got %+v, want one call with %+v", got, want)
	}
}

// TestDiskForRefuses: every way DiskFor can fail refuses the upload before
// a byte is stored, with the error's own status or 500.
func TestDiskForRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		disk   string
		err    error
		status int
		code   filesystem.Code
	}{
		// A name returned beside an error is not used.
		"coded error":   {disk: "media", err: filesystem.Errorf(filesystem.CodePermission, "tenant has no bucket"), status: http.StatusForbidden, code: filesystem.CodePermission},
		"uncoded error": {disk: "media", err: errors.New("lookup failed"), status: http.StatusInternalServerError, code: filesystem.CodeUnknown},
		"empty name":    {status: http.StatusInternalServerError, code: filesystem.CodeUnknown},
		"unknown disk":  {disk: "nope", status: http.StatusInternalServerError, code: filesystem.CodeUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			e := newRouteEnv(t, recordshttp.Config{DiskFor: func(*http.Request, recordshttp.Target) (string, error) { return tc.disk, tc.err }})
			rec := e.upload(t)
			wantStatus(t, rec, tc.status)
			if er := decode[recordshttp.ErrorResponse](t, rec); er.Code != tc.code {
				t.Fatalf("code = %s, want %s", er.Code, tc.code)
			}
			if c := e.counts(t); len(c) != 0 {
				t.Fatalf("objects stored after a refusal: %v", c)
			}
			page, err := e.store.Files(t.Context(), records.FileQuery{Scope: tenant})
			if err != nil || len(page.Files) != 0 {
				t.Fatalf("rows after a refusal: %v, %v", page.Files, err)
			}
		})
	}
}

// TestDiskByVisibility: with no Disk and no DiskFor, a Store built
// WithVisibilityDisks chooses by the policy's visibility.
func TestDiskByVisibility(t *testing.T) {
	for vis, want := range map[filesystem.Visibility]string{
		filesystem.VisibilityPublic:  "pub",
		filesystem.VisibilityPrivate: "priv",
		"":                           "priv",
	} {
		t.Run(string(vis)+"→"+want, func(t *testing.T) {
			e := newRouteEnv(t, recordshttp.Config{Policy: upload.Policy{MaxSize: 1 << 10, Allowed: []string{"text/plain"}, Visibility: vis}},
				records.WithVisibilityDisks("pub", "priv"))
			if d := e.uploadedDisk(t); d != want {
				t.Fatalf("disk = %q, want %q", d, want)
			}
		})
	}
}

func TestDiskConfig(t *testing.T) {
	scope := func(*http.Request) (string, error) { return "", nil }
	auth := func(*http.Request, recordshttp.Action, recordshttp.Target) error { return nil }
	diskFor := func(*http.Request, recordshttp.Target) (string, error) { return "media", nil }
	policy := upload.Policy{MaxSize: 1, Allowed: []string{"text/plain"}}
	mgr, err := filesystem.NewManager()
	if err != nil {
		t.Fatal(err)
	}
	plain, err := records.New(testdb.Open(t), testdb.Dialect(), mgr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recordshttp.Handler(recordshttp.Config{Store: plain, Disk: "media", DiskFor: diskFor, Policy: policy, Scope: scope, Authorize: auth}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("Disk and DiskFor: err = %v", err)
	}
	if _, err := recordshttp.Handler(recordshttp.Config{Store: plain, Policy: policy, Scope: scope, Authorize: auth}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("no disk, no DiskFor, no visibility disks: err = %v", err)
	}
	if _, err := recordshttp.Handler(recordshttp.Config{Store: plain, DiskFor: diskFor, Policy: policy, Scope: scope, Authorize: auth}); err != nil {
		t.Fatalf("DiskFor alone: %v", err)
	}
}
