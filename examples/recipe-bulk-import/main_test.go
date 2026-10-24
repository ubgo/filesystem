package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem/records"
)

func setup(t *testing.T) (*App, string) {
	t.Helper()
	app, err := New(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Handler)
	t.Cleanup(srv.Close)
	return app, srv.URL
}

// send uploads a CSV as tenant and returns the status and body.
func send(t *testing.T, base, tenant, name, data string) (int, importResponse) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	w, _ := mw.CreateFormFile("file", name)
	_, _ = w.Write([]byte(data))
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, base+"/imports", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(headerTenant, tenant)
	return getJSON[importResponse](t, req)
}

// get sends a GET as tenant and returns the status and raw body.
func get(t *testing.T, base, tenant, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+path, nil)
	req.Header.Set(headerTenant, tenant)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func getJSON[T any](t *testing.T, req *http.Request) (int, T) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out T
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

const goodCSV = "name,email\nAda Lovelace,ada@example.com\nAlan Turing,ALAN@example.com\nNo Address,not-an-email\n,blank@example.com\n"

func TestImport(t *testing.T) {
	app, base := setup(t)
	code, accepted := send(t, base, "t1", "contacts.csv", goodCSV)
	if code != http.StatusAccepted || accepted.Status != records.StatusProcessing || accepted.Detail != detailQueued {
		t.Fatalf("accept = %d %+v", code, accepted)
	}
	if code, body := get(t, base, "t1", "/imports/"+accepted.ImportID+"/report"); code != http.StatusNotFound {
		t.Errorf("report before the job = %d %s", code, body)
	}

	job := app.ImportJob()
	res, err := job.Run(t.Context(), time.Now())
	if err != nil || res.Count != 1 {
		t.Fatalf("job = %+v, %v", res, err)
	}
	_, body := get(t, base, "t1", "/imports/"+accepted.ImportID)
	if !strings.Contains(body, `"status":"ready"`) || !strings.Contains(body, "imported 2, refused 2") {
		t.Errorf("status = %s", body)
	}
	_, body = get(t, base, "t1", "/contacts")
	if body != `[{"name":"Ada Lovelace","email":"ada@example.com"},{"name":"Alan Turing","email":"alan@example.com"}]`+"\n" {
		t.Errorf("contacts = %s", body)
	}
	_, body = get(t, base, "t1", "/imports/"+accepted.ImportID+"/report")
	want := "line,reason,name,email\n4,email is not an address,No Address,not-an-email\n5,name is empty,,blank@example.com\n"
	if body != want {
		t.Errorf("report = %q", body)
	}

	// Another run finds nothing queued; a tenant cannot see another's import.
	if res, err := job.Run(t.Context(), time.Now()); err != nil || res.Count != 0 {
		t.Errorf("second run = %+v, %v", res, err)
	}
	if code, _ := get(t, base, "t2", "/imports/"+accepted.ImportID); code != http.StatusNotFound {
		t.Errorf("other tenant = %d", code)
	}
	if _, body := get(t, base, "t2", "/contacts"); body != "[]\n" {
		t.Errorf("other tenant's contacts = %s", body)
	}
}

func TestBadFiles(t *testing.T) {
	app, base := setup(t)
	_, wrongHeader := send(t, base, "t1", "contacts.csv", "email,name\na@example.com,A\n")
	if _, err := app.ImportJob().Run(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	_, body := get(t, base, "t1", "/imports/"+wrongHeader.ImportID)
	if !strings.Contains(body, `"status":"failed"`) || !strings.Contains(body, `line 1 must be the header \"name,email\"`) {
		t.Errorf("wrong header = %s", body)
	}
	if code, _ := send(t, base, "t1", "contacts.csv", ""); code != http.StatusUnsupportedMediaType {
		t.Errorf("empty file = %d", code)
	}
	if code, out := send(t, base, "t1", "contacts.xlsx", "PK\x03\x04"); code != http.StatusUnsupportedMediaType {
		t.Errorf("not a CSV = %d %+v", code, out)
	}
	if code, _ := send(t, base, "", "contacts.csv", goodCSV); code != http.StatusForbidden {
		t.Errorf("anonymous = %d", code)
	}
}
