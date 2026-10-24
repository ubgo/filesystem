package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/records/recordshttp"
)

// client calls the server as one user of one tenant.
type client struct {
	t      *testing.T
	srv    *httptest.Server
	tenant string
	user   string
}

func newClient(t *testing.T) client {
	t.Helper()
	h, closeDB, err := build(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeDB)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return client{t: t, srv: srv, tenant: "acme", user: "u1"}
}

// do sends one request without following redirects.
func (c client) do(method, path, contentType string, body io.Reader) (*http.Response, []byte) {
	c.t.Helper()
	req, _ := http.NewRequestWithContext(c.t.Context(), method, c.srv.URL+path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.tenant != "" {
		req.Header.Set(headerTenant, c.tenant)
	}
	req.Header.Set(headerUser, c.user)
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		c.t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, data
}

// form sends files (name → content) in the multipart field.
func (c client) form(method, path string, files map[string]string) (*http.Response, []byte) {
	c.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for name, content := range files {
		w, _ := mw.CreateFormFile(recordshttp.FormField, name)
		_, _ = w.Write([]byte(content))
	}
	_ = mw.Close()
	return c.do(method, path, mw.FormDataContentType(), &body)
}

func decodeList(t *testing.T, data []byte) recordshttp.ListResponse {
	t.Helper()
	var lr recordshttp.ListResponse
	if err := json.Unmarshal(data, &lr); err != nil {
		t.Fatalf("%v in %s", err, data)
	}
	return lr
}

func TestEndpoints(t *testing.T) {
	c := newClient(t)
	resp, data := c.form(http.MethodPost, "/media/owners/user/u1/gallery", map[string]string{"a.txt": "alpha", "b.txt": "bravo"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d %s", resp.StatusCode, data)
	}
	up := decodeList(t, data)
	if len(up.Files) != 2 || up.Files[0].UploadedBy != "user:u1" {
		t.Fatalf("upload = %+v", up)
	}
	ids := []string{up.Files[1].AttachmentID, up.Files[0].AttachmentID}
	body, _ := json.Marshal(recordshttp.ReorderRequest{IDs: ids})
	if resp, data := c.do(http.MethodPut, "/media/owners/user/u1/gallery/order", "application/json", bytes.NewReader(body)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reorder = %d %s", resp.StatusCode, data)
	}
	_, data = c.do(http.MethodGet, "/media/owners/user/u1/gallery", "", nil)
	list := decodeList(t, data)
	if list.Files[0].AttachmentID != ids[0] || list.Files[0].URL == "" {
		t.Fatalf("list after reorder = %+v", list)
	}
	fileID := list.Files[0].FileID
	resp, _ = c.do(http.MethodGet, "/media/files/"+fileID, "", nil)
	loc, err := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || err != nil {
		t.Fatalf("read = %d %v", resp.StatusCode, err)
	}
	if _, data := c.do(http.MethodGet, loc.RequestURI(), "", nil); len(data) == 0 {
		t.Fatal("the signed URL served nothing")
	}
	if resp, data := c.form(http.MethodPut, "/media/files/"+fileID, map[string]string{"v2.txt": "new content"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("replace = %d %s", resp.StatusCode, data)
	}
	if resp, _ := c.do(http.MethodDelete, "/media/attachments/"+ids[1], "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("detach = %d", resp.StatusCode)
	}
	if resp, _ := c.do(http.MethodDelete, "/media/files/"+fileID, "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("trash = %d", resp.StatusCode)
	}
	if resp, _ := c.do(http.MethodGet, "/media/files/"+fileID, "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("read trashed = %d", resp.StatusCode)
	}
}

func TestRefusals(t *testing.T) {
	c := newClient(t)
	resp, data := c.form(http.MethodPost, "/media/owners/user/u1/gallery", map[string]string{"a.txt": "alpha"})
	fileID := decodeList(t, data).Files[0].FileID
	anon := c
	anon.tenant = ""
	other := c
	other.user = "u2"
	cases := []struct {
		name string
		resp *http.Response
		want int
	}{
		{"no tenant", first(anon.do(http.MethodGet, "/media/owners/user/u1/gallery", "", nil)), http.StatusUnauthorized},
		{"someone else's owner", first(other.form(http.MethodPost, "/media/owners/user/u1/gallery", map[string]string{"x.txt": "x"})), http.StatusForbidden},
		{"someone else's file", first(other.do(http.MethodDelete, "/media/files/"+fileID, "", nil)), http.StatusForbidden},
		{"active content", first(c.form(http.MethodPost, "/media/owners/user/u1/gallery", map[string]string{"p.html": "<html><script></script></html>"})), http.StatusUnsupportedMediaType},
		{"too large", first(c.form(http.MethodPost, "/media/owners/user/u1/gallery", map[string]string{"big.txt": strings.Repeat("a", maxFileSize+1)})), http.StatusRequestEntityTooLarge},
		{"bad reorder", first(c.do(http.MethodPut, "/media/owners/user/u1/gallery/order", "application/json", strings.NewReader(`{"ids":[]}`))), http.StatusBadRequest},
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d", resp.StatusCode)
	}
	for _, tc := range cases {
		if tc.resp.StatusCode != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.resp.StatusCode, tc.want)
		}
	}
}

// TestUploadIsAllOrNothing sends more files than MaxFiles: the files
// stored before the refusal are rolled back with their objects.
func TestUploadIsAllOrNothing(t *testing.T) {
	c := newClient(t)
	files := map[string]string{"1.txt": "1", "2.txt": "2", "3.txt": "3", "4.txt": "4"}
	if resp, _ := c.form(http.MethodPost, "/media/owners/user/u1/gallery", files); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("upload of 4 = %d", resp.StatusCode)
	}
	_, data := c.do(http.MethodGet, "/media/owners/user/u1/gallery", "", nil)
	if n := len(decodeList(t, data).Files); n != 0 {
		t.Fatalf("%d files attached after a refused upload", n)
	}
}

func first(resp *http.Response, _ []byte) *http.Response { return resp }

// TestDiskPerTenant uploads through the /byo routes: globex's file lands
// on its own bucket (its signed link names that disk and serves the
// bytes), and a tenant without a bucket is refused with nothing stored.
func TestDiskPerTenant(t *testing.T) {
	c := newClient(t)
	c.tenant = "globex"
	resp, data := c.form(http.MethodPost, "/byo/owners/user/u1/gallery", map[string]string{"plan.txt": "quarterly plan"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("globex upload = %d %s", resp.StatusCode, data)
	}
	_, data = c.do(http.MethodGet, "/media/owners/user/u1/gallery", "", nil)
	list := decodeList(t, data)
	if len(list.Files) != 1 || !strings.Contains(list.Files[0].URL, "/files/globex-bucket/uploads/") {
		t.Fatalf("globex list = %s", data)
	}
	loc, _ := url.Parse(list.Files[0].URL)
	if _, body := c.do(http.MethodGet, loc.RequestURI(), "", nil); string(body) != "quarterly plan" {
		t.Fatalf("download from the tenant's bucket = %q", body)
	}
	acme := c
	acme.tenant = "acme"
	resp, data = acme.form(http.MethodPost, "/byo/owners/user/u1/gallery", map[string]string{"plan.txt": "quarterly plan"})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(data), `"code":"permission"`) {
		t.Fatalf("acme upload = %d %s", resp.StatusCode, data)
	}
	_, data = acme.do(http.MethodGet, "/media/owners/user/u1/gallery", "", nil)
	if n := len(decodeList(t, data).Files); n != 0 {
		t.Fatalf("acme has %d files after a refused upload", n)
	}
}
