package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/upload"
)

// textBody is a file every route accepts: plain text is in the policy.
const textBody = "quarterly numbers\n"

// htmlBody is detected as text/html, which every policy refuses unless it
// sets AllowActiveContent.
const htmlBody = "<!doctype html><script>alert(1)</script>"

// router builds a router over the app; both routers must behave alike.
type router func(Handlers) http.Handler

func TestServeMux(t *testing.T) {
	exercise(t, func(h Handlers) http.Handler { return newServeMux(h) })
}

func TestGorilla(t *testing.T) {
	exercise(t, func(h Handlers) http.Handler { return newGorilla(h) })
}

// exercise runs every route through the router: the same requests must
// give the same answers whichever router mounts the handlers.
func exercise(t *testing.T, mount router) {
	s3srv := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
	t.Cleanup(s3srv.Close)
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	app, err := newApp(t.Context(), srv.URL, s3srv.URL, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h = mount(app.Handlers)
	t.Run("form upload", func(t *testing.T) { formUpload(t, srv.URL) })
	t.Run("records", func(t *testing.T) { recordsFlow(t, srv.URL) })
	t.Run("public", func(t *testing.T) { publicFile(t, app, srv.URL) })
	t.Run("ticket", func(t *testing.T) { ticket(t, srv.URL) })
	t.Run("uppy", func(t *testing.T) { uppy(t, srv.URL) })
	t.Run("tus", func(t *testing.T) { tusUpload(t, app, srv.URL) })
}

// form returns a multipart body with one file in the "file" field.
func form(t *testing.T, name, content string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(fw, content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

// do sends a request and returns the status, body and response.
func do(t *testing.T, method, url, contentType, user string, body io.Reader) (int, string, *http.Response) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if user != "" {
		req.Header.Set(headerUser, user)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b), resp
}

func formUpload(t *testing.T, base string) {
	body, ct := form(t, "report.txt", textBody)
	status, out, _ := do(t, http.MethodPost, base+pathUpload, ct, "", body)
	var res upload.Response
	if err := json.Unmarshal([]byte(out), &res); err != nil || status != http.StatusCreated || len(res.Files) != 1 || res.Files[0].ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("upload = %d %s", status, out)
	}
	body, ct = form(t, "page.html", htmlBody)
	if status, out, _ := do(t, http.MethodPost, base+pathUpload, ct, "", body); status != http.StatusUnsupportedMediaType || !strings.Contains(out, `"code":"rejected"`) {
		t.Errorf("html upload = %d %s", status, out)
	}
}

func recordsFlow(t *testing.T, base string) {
	owner := base + pathRecords + "/owners/user/alice/docs"
	body, ct := form(t, "notes.txt", textBody)
	if status, out, _ := do(t, http.MethodPost, owner, ct, "", body); status != http.StatusUnauthorized {
		t.Errorf("signed out = %d %s", status, out)
	}
	body, ct = form(t, "notes.txt", textBody)
	if status, out, _ := do(t, http.MethodPost, owner, ct, "bob", body); status != http.StatusForbidden {
		t.Errorf("another user = %d %s", status, out)
	}
	body, ct = form(t, "notes.txt", textBody)
	status, out, _ := do(t, http.MethodPost, owner, ct, "alice", body)
	var attached struct {
		Files []struct {
			FileID string `json:"file_id"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &attached); err != nil || status != http.StatusCreated || len(attached.Files) != 1 {
		t.Fatalf("attach = %d %s", status, out)
	}
	status, _, resp := do(t, http.MethodGet, base+pathRecords+"/files/"+attached.Files[0].FileID, "", "alice", nil)
	if status != http.StatusFound {
		t.Fatalf("read = %d", status)
	}
	if status, got, _ := do(t, http.MethodGet, resp.Header.Get("Location"), "", "", nil); status != http.StatusOK || got != textBody {
		t.Errorf("signed link = %d %q", status, got)
	}
}

func publicFile(t *testing.T, app *App, base string) {
	ctx := t.Context()
	if _, err := app.Disks.Public.WriteBytes(ctx, "logos/app.txt", []byte(textBody), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	link, err := app.Disks.Public.PublicURL(ctx, "logos/app.txt")
	if err != nil || link != base+pathPublic+"/logos/app.txt" {
		t.Fatalf("public URL = %q, %v", link, err)
	}
	if status, got, _ := do(t, http.MethodGet, link, "", "", nil); status != http.StatusOK || got != textBody {
		t.Errorf("public file = %d %q", status, got)
	}
}

func ticket(t *testing.T, base string) {
	req := `{"filename":"scan.pdf","content_type":"application/pdf","size":1024}`
	status, out, _ := do(t, http.MethodPost, base+pathTicket, "application/json", "", strings.NewReader(req))
	var tk upload.Ticket
	if err := json.Unmarshal([]byte(out), &tk); err != nil || status != http.StatusOK || tk.Method != http.MethodPut || !strings.HasPrefix(tk.Key, "incoming/") {
		t.Fatalf("ticket = %d %s", status, out)
	}
	req = `{"filename":"page.html","content_type":"text/html","size":10}`
	if status, out, _ := do(t, http.MethodPost, base+pathTicket, "application/json", "", strings.NewReader(req)); status != http.StatusUnsupportedMediaType {
		t.Errorf("html ticket = %d %s", status, out)
	}
}

// uppy walks one file through Uppy's protocol: create, sign part 1, send
// it to the bucket, complete.
func uppy(t *testing.T, base string) {
	root := base + pathUppy + "/multipart"
	status, out, _ := do(t, http.MethodPost, root, "application/json", "alice", strings.NewReader(`{"filename":"notes.txt","type":"text/plain"}`))
	var created struct{ UploadID, Key string }
	if err := json.Unmarshal([]byte(out), &created); err != nil || status != http.StatusOK {
		t.Fatalf("create = %d %s", status, out)
	}
	q := "?key=" + created.Key
	status, out, _ = do(t, http.MethodGet, root+"/"+created.UploadID+"/1"+q, "", "alice", nil)
	var signed struct{ URL string }
	if err := json.Unmarshal([]byte(out), &signed); err != nil || status != http.StatusOK {
		t.Fatalf("sign = %d %s", status, out)
	}
	_, _, resp := do(t, http.MethodPut, signed.URL, "", "", strings.NewReader(textBody))
	parts := `{"parts":[{"PartNumber":1,"ETag":` + jsonString(resp.Header.Get("ETag")) + `}]}`
	status, out, _ = do(t, http.MethodPost, root+"/"+created.UploadID+"/complete"+q, "application/json", "alice", strings.NewReader(parts))
	if status != http.StatusOK || !strings.Contains(out, created.Key) {
		t.Errorf("complete = %d %s", status, out)
	}
	if status, _, _ := do(t, http.MethodGet, root+"/"+created.UploadID+q, "", "", nil); status != http.StatusForbidden {
		t.Errorf("list parts without a user = %d", status)
	}
}

// jsonString quotes s as a JSON string.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// tusUpload creates a tus upload and sends it in one PATCH; the response
// names the stored key.
func tusUpload(t *testing.T, app *App, base string) {
	meta := "filename " + base64.StdEncoding.EncodeToString([]byte("notes.txt")) + ",filetype " + base64.StdEncoding.EncodeToString([]byte("text/plain"))
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, base+pathTus, nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", "18")
	req.Header.Set("Upload-Metadata", meta)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %v %v", resp, err)
	}
	_ = resp.Body.Close()
	patch, _ := http.NewRequestWithContext(t.Context(), http.MethodPatch, resp.Header.Get("Location"), strings.NewReader(textBody))
	patch.Header.Set("Tus-Resumable", "1.0.0")
	patch.Header.Set("Upload-Offset", "0")
	patch.Header.Set("Content-Type", "application/offset+octet-stream")
	resp, err = http.DefaultClient.Do(patch)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("patch = %v %v", resp, err)
	}
	_ = resp.Body.Close()
	got, err := app.Disks.Media.ReadAll(t.Context(), resp.Header.Get("Filesystem-Key"))
	if err != nil || string(got) != textBody {
		t.Errorf("stored = %q, %v", got, err)
	}
}

func TestStartFakeS3(t *testing.T) {
	u, stop, err := startFakeS3()
	if err != nil || !strings.HasPrefix(u, "http://127.0.0.1:") {
		t.Fatalf("%q %v", u, err)
	}
	stop()
}
