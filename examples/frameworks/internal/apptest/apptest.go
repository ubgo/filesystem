// Package apptest drives every mounted endpoint of an app over real HTTP,
// so each framework's test proves its routing passes the library's
// handlers the requests they expect: methods, paths with the prefix
// stripped, multipart bodies, redirects and tus's PATCH. ExerciseAdapters
// does the same for the adapter examples under adapters/.
package apptest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/examples/frameworks/internal/app"
)

// TextBody is a file every route accepts.
const TextBody = "quarterly numbers\n"

// htmlBody is refused by every policy that does not allow active content.
const htmlBody = "<!doctype html><script>alert(1)</script>"

// Listen opens a listener on a free local port and returns it with the
// base URL clients reach it at.
func Listen(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln, "http://" + ln.Addr().String()
}

// NewApp wires an app for base, on a fake S3 server stopped when the test
// ends.
func NewApp(t *testing.T, base string) *app.App {
	t.Helper()
	s3URL, stop, err := app.StartFakeS3()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	a, err := app.New(t.Context(), base, s3URL, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Serve serves h on ln until the test ends.
func Serve(t *testing.T, ln net.Listener, h http.Handler) {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// Exercise runs every route of a through the server at base.
// ds:def id=exercise-u7ra8xnm
func Exercise(t *testing.T, base string, a *app.App) {
	t.Run("form upload", func(t *testing.T) { formUpload(t, base) })
	t.Run("records", func(t *testing.T) { recordsFlow(t, base) })
	t.Run("public", func(t *testing.T) { publicFile(t, a, base) })
	t.Run("ticket", func(t *testing.T) { ticket(t, base) })
	t.Run("uppy", func(t *testing.T) { uppy(t, base) })
	t.Run("tus", func(t *testing.T) { tusUpload(t, a, base) })
}

// Form returns a multipart body with one file in the "file" field.
func Form(t *testing.T, name, content string) (*bytes.Buffer, string) {
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

// Do sends a request without following redirects and returns the status,
// body and response.
func Do(t *testing.T, method, url string, header http.Header, body io.Reader) (int, string, *http.Response) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
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

// headers builds a header set from name, value pairs.
func headers(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			h.Set(kv[i], kv[i+1])
		}
	}
	return h
}

func formUpload(t *testing.T, base string) {
	body, ct := Form(t, "report.txt", TextBody)
	status, out, _ := Do(t, http.MethodPost, base+app.PathUpload, headers("Content-Type", ct), body)
	if status != http.StatusCreated || !strings.Contains(out, `"content_type":"text/plain; charset=utf-8"`) {
		t.Fatalf("upload = %d %s", status, out)
	}
	body, ct = Form(t, "page.html", htmlBody)
	if status, out, _ := Do(t, http.MethodPost, base+app.PathUpload, headers("Content-Type", ct), body); status != http.StatusUnsupportedMediaType || !strings.Contains(out, `"code":"rejected"`) {
		t.Errorf("html upload = %d %s", status, out)
	}
}

func recordsFlow(t *testing.T, base string) {
	owner := base + app.PathRecords + "/owners/user/alice/docs"
	for user, want := range map[string]int{"": http.StatusUnauthorized, "bob": http.StatusForbidden} {
		body, ct := Form(t, "notes.txt", TextBody)
		if status, out, _ := Do(t, http.MethodPost, owner, headers("Content-Type", ct, app.HeaderUser, user), body); status != want {
			t.Errorf("user %q = %d %s", user, status, out)
		}
	}
	body, ct := Form(t, "notes.txt", TextBody)
	status, out, _ := Do(t, http.MethodPost, owner, headers("Content-Type", ct, app.HeaderUser, "alice"), body)
	var attached struct {
		Files []struct {
			FileID string `json:"file_id"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &attached); err != nil || status != http.StatusCreated || len(attached.Files) != 1 {
		t.Fatalf("attach = %d %s", status, out)
	}
	status, _, resp := Do(t, http.MethodGet, base+app.PathRecords+"/files/"+attached.Files[0].FileID, headers(app.HeaderUser, "alice"), nil)
	if status != http.StatusFound {
		t.Fatalf("read = %d", status)
	}
	if status, got, _ := Do(t, http.MethodGet, resp.Header.Get("Location"), nil, nil); status != http.StatusOK || got != TextBody {
		t.Errorf("signed link = %d %q", status, got)
	}
}

func publicFile(t *testing.T, a *app.App, base string) {
	ctx := t.Context()
	if _, err := a.Disks.Public.WriteBytes(ctx, "logos/app.txt", []byte(TextBody), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	link, err := a.Disks.Public.PublicURL(ctx, "logos/app.txt")
	if err != nil || link != base+app.PathPublic+"/logos/app.txt" {
		t.Fatalf("public URL = %q, %v", link, err)
	}
	if status, got, _ := Do(t, http.MethodGet, link, nil, nil); status != http.StatusOK || got != TextBody {
		t.Errorf("public file = %d %q", status, got)
	}
}

func ticket(t *testing.T, base string) {
	req := `{"filename":"scan.pdf","content_type":"application/pdf","size":1024}`
	status, out, _ := Do(t, http.MethodPost, base+app.PathTicket, headers("Content-Type", "application/json"), strings.NewReader(req))
	if status != http.StatusOK || !strings.Contains(out, `"method":"PUT"`) {
		t.Fatalf("ticket = %d %s", status, out)
	}
}

// uppy walks one file through Uppy's protocol: create, sign part 1, send
// it to the bucket, complete.
func uppy(t *testing.T, base string) {
	root := base + app.PathUppy + "/multipart"
	user := headers(app.HeaderUser, "alice", "Content-Type", "application/json")
	status, out, _ := Do(t, http.MethodPost, root, user, strings.NewReader(`{"filename":"notes.txt","type":"text/plain"}`))
	var created struct{ UploadID, Key string }
	if err := json.Unmarshal([]byte(out), &created); err != nil || status != http.StatusOK {
		t.Fatalf("create = %d %s", status, out)
	}
	q := "?key=" + created.Key
	status, out, _ = Do(t, http.MethodGet, root+"/"+created.UploadID+"/1"+q, user, nil)
	var signed struct{ URL string }
	if err := json.Unmarshal([]byte(out), &signed); err != nil || status != http.StatusOK {
		t.Fatalf("sign = %d %s", status, out)
	}
	_, _, resp := Do(t, http.MethodPut, signed.URL, nil, strings.NewReader(TextBody))
	etag, _ := json.Marshal(resp.Header.Get("ETag"))
	parts := `{"parts":[{"PartNumber":1,"ETag":` + string(etag) + `}]}`
	status, out, _ = Do(t, http.MethodPost, root+"/"+created.UploadID+"/complete"+q, user, strings.NewReader(parts))
	if status != http.StatusOK || !strings.Contains(out, created.Key) {
		t.Errorf("complete = %d %s", status, out)
	}
}

// tusUpload creates a tus upload and sends it in one PATCH; the response
// names the stored key.
func tusUpload(t *testing.T, a *app.App, base string) {
	meta := "filename " + base64.StdEncoding.EncodeToString([]byte("notes.txt")) + ",filetype " + base64.StdEncoding.EncodeToString([]byte("text/plain"))
	status, _, resp := Do(t, http.MethodPost, base+app.PathTus, headers("Tus-Resumable", "1.0.0", "Upload-Length", "18", "Upload-Metadata", meta), nil)
	if status != http.StatusCreated {
		t.Fatalf("create = %d", status)
	}
	h := headers("Tus-Resumable", "1.0.0", "Upload-Offset", "0", "Content-Type", "application/offset+octet-stream")
	status, _, resp = Do(t, http.MethodPatch, resp.Header.Get("Location"), h, strings.NewReader(TextBody))
	if status != http.StatusNoContent {
		t.Fatalf("patch = %d", status)
	}
	got, err := a.Disks.Media.ReadAll(t.Context(), resp.Header.Get("Filesystem-Key"))
	if err != nil || string(got) != TextBody {
		t.Errorf("stored = %q, %v", got, err)
	}
}
