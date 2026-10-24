package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
)

type env struct {
	app  *App
	base string
	dir  string
}

func setup(t *testing.T) env {
	t.Helper()
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	app, err := New(t.Context(), dir, srv.URL, bytes.Repeat([]byte("k"), 32), demoEnv(dir))
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = app.Handler
	return env{app: app, base: srv.URL, dir: dir}
}

// do sends a request with an API key and returns the status and body.
func (e env) do(t *testing.T, method, path, key, contentType string, body io.Reader) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, e.base+path, body)
	req.Header.Set("Authorization", "Bearer "+key)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// upload posts a text file as key's tenant and returns the stored file.
func (e env) upload(t *testing.T, key, name, content string) (int, fileResponse) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = io.WriteString(fw, content)
	_ = mw.Close()
	status, data := e.do(t, http.MethodPost, "/files", key, mw.FormDataContentType(), &body)
	var f fileResponse
	_ = json.Unmarshal(data, &f)
	return status, f
}

func TestTenantIsolation(t *testing.T) {
	e := setup(t)
	status, f := e.upload(t, "key-acme", "plan.txt", "acme's plan")
	if status != http.StatusCreated || f.Disk != sharedDisk || !strings.HasPrefix(f.Key, "tenants/acme/files/") {
		t.Fatalf("acme upload = %d %+v", status, f)
	}
	if status, _ := e.do(t, http.MethodGet, "/files/"+f.ID, "key-globex", "", nil); status != http.StatusNotFound {
		t.Errorf("globex reading acme's file = %d, want 404", status)
	}
	if status, _ := e.do(t, http.MethodGet, "/files/"+f.ID, "key-acme", "", nil); status != http.StatusOK {
		t.Errorf("acme reading its file (after redirect) = %d", status)
	}
	_, data := e.do(t, http.MethodGet, "/files", "key-globex", "", nil)
	if strings.Contains(string(data), f.ID) {
		t.Errorf("globex's listing shows acme's file: %s", data)
	}
	if status, _ := e.do(t, http.MethodGet, "/files", "nope", "", nil); status != http.StatusForbidden {
		t.Errorf("unknown key = %d", status)
	}
	// globex has a disk of its own in the configuration.
	if _, g := e.upload(t, "key-globex", "g.txt", "globex"); g.Disk != dedicatedPrefix+"globex" {
		t.Errorf("globex stored on %q", g.Disk)
	}
}

func TestQuotaPerPlan(t *testing.T) {
	e := setup(t)
	for i := range 3 {
		if status, _ := e.upload(t, "key-acme", "n.txt", "note"); status != http.StatusCreated {
			t.Fatalf("upload %d = %d", i, status)
		}
	}
	if status, _ := e.upload(t, "key-acme", "n.txt", "note"); status != http.StatusInsufficientStorage {
		t.Errorf("fourth file on the free plan = %d, want 507", status)
	}
	if status, _ := e.upload(t, "key-globex", "n.txt", "note"); status != http.StatusCreated {
		t.Errorf("pro tenant = %d", status)
	}
}

func TestScopedDiskAndOffboarding(t *testing.T) {
	e := setup(t)
	if status, _ := e.do(t, http.MethodPut, "/settings/theme", "key-acme", "application/json", strings.NewReader(`{"color":"teal"}`)); status != http.StatusOK {
		t.Fatalf("settings = %d", status)
	}
	disk, _ := e.app.tenantDisk("acme")
	if _, err := disk.WriteBytes(t.Context(), "../globex/x", []byte("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("traversal out of the tenant prefix = %v", err)
	}
	e.upload(t, "key-acme", "a.txt", "a")
	_, keep := e.upload(t, "key-globex", "b.txt", "b")

	if status, _ := e.do(t, http.MethodDelete, "/admin/tenants/acme", "key-acme", "", nil); status != http.StatusForbidden {
		t.Errorf("tenant offboarding itself = %d", status)
	}
	status, data := e.do(t, http.MethodDelete, "/admin/tenants/acme", "key-admin", "", nil)
	if status != http.StatusOK || !strings.Contains(string(data), `"files_purged":1`) {
		t.Fatalf("offboard = %d %s", status, data)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "shared", "tenants", "acme")); !os.IsNotExist(err) {
		t.Errorf("acme's prefix survived: %v", err)
	}
	if status, _ := e.do(t, http.MethodGet, "/files/"+keep.ID, "key-globex", "", nil); status != http.StatusOK {
		t.Errorf("globex lost its file: %d", status)
	}
	if u, _ := e.app.Quota.Usage(t.Context(), e.app.Store, "acme"); u.Files != 0 || u.Bytes != 0 {
		t.Errorf("acme usage after offboarding = %+v", u)
	}
}
