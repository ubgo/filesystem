package upload_test

import (
	"bytes"
	"context"
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
	"github.com/ubgo/filesystem/upload"
)

type formFile struct{ field, name, data string }

func multipartBody(t *testing.T, files []formFile, fields map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	for _, f := range files {
		w, err := mw.CreateFormFile(f.field, f.name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, f.data)
	}
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func post(t *testing.T, h http.Handler, body io.Reader, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/upload", body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorBody(t *testing.T, rec *httptest.ResponseRecorder) upload.ErrorResponse {
	t.Helper()
	var e upload.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body %q: %v", rec.Body.String(), err)
	}
	return e
}

func count(t *testing.T, d *filesystem.Disk) int {
	t.Helper()
	n := 0
	for _, err := range d.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	return n
}

func TestHandlerStoresFiles(t *testing.T) {
	d := newDisk(t, memory.New())
	var stored []upload.Result
	h := upload.Handler(d, images, upload.HandlerOptions{MaxFiles: 2, OnStored: func(_ context.Context, _ *http.Request, files []upload.Result) error {
		stored = files
		return nil
	}})
	body, ct := multipartBody(t, []formFile{{"file", "a.png", pngData}, {"other", "ignored.png", pngData}, {"file", "b.png", pngData}}, map[string]string{"caption": "hi"})
	rec := post(t, h, body, ct)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp upload.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Files) != 2 || resp.Files[0].Filename != "a.png" {
		t.Fatalf("response %s (%v)", rec.Body, err)
	}
	if len(stored) != 2 || count(t, d) != 2 {
		t.Errorf("OnStored saw %d, disk has %d", len(stored), count(t, d))
	}
}

func TestHandlerIsAllOrNothing(t *testing.T) {
	cases := []struct {
		name    string
		files   []formFile
		opts    upload.HandlerOptions
		status  int
		code    filesystem.Code
		message string
	}{
		{"second file rejected", []formFile{{"file", "a.png", pngData}, {"file", "b.html", htmlData}}, upload.HandlerOptions{MaxFiles: 2}, 415, filesystem.CodeRejected, "is not allowed"},
		{"too many files", []formFile{{"file", "a.png", pngData}, {"file", "b.png", pngData}}, upload.HandlerOptions{}, 400, filesystem.CodeInvalidArgument, "more than 1 files"},
		{"hook fails", []formFile{{"file", "a.png", pngData}}, upload.HandlerOptions{OnStored: func(context.Context, *http.Request, []upload.Result) error {
			return errors.New("database is down: password=hunter2")
		}}, 500, filesystem.CodeUnknown, "Internal Server Error"},
		{"no file", nil, upload.HandlerOptions{}, 400, filesystem.CodeInvalidArgument, `no file in field "file"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newDisk(t, memory.New())
			body, ct := multipartBody(t, c.files, nil)
			rec := post(t, upload.Handler(d, images, c.opts), body, ct)
			e := errorBody(t, rec)
			if rec.Code != c.status || e.Code != c.code || !strings.Contains(e.Error, c.message) {
				t.Errorf("got %d %+v, want %d %s %q", rec.Code, e, c.status, c.code, c.message)
			}
			if strings.Contains(rec.Body.String(), "hunter2") {
				t.Error("hook error leaked to the client")
			}
			if n := count(t, d); n != 0 {
				t.Errorf("%d files left after a failed request", n)
			}
		})
	}
}

func TestHandlerRequestErrors(t *testing.T) {
	d := newDisk(t, memory.New())
	h := upload.Handler(d, images, upload.HandlerOptions{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/upload", nil))
	if rec.Code != 405 || rec.Header().Get("Allow") != http.MethodPost {
		t.Errorf("GET = %d", rec.Code)
	}
	if rec := post(t, h, strings.NewReader("x"), "application/json"); rec.Code != 400 {
		t.Errorf("non-multipart = %d", rec.Code)
	}
	if rec := post(t, h, strings.NewReader("--x\r\nbroken"), "multipart/form-data; boundary=x"); rec.Code != 400 && rec.Code != 500 {
		t.Errorf("malformed multipart = %d", rec.Code)
	}
	bad := upload.Handler(d, upload.Policy{}, upload.HandlerOptions{})
	body, ct := multipartBody(t, []formFile{{"file", "a.png", pngData}}, nil)
	if rec := post(t, bad, body, ct); rec.Code != 500 {
		t.Errorf("invalid policy = %d", rec.Code)
	}
	// A body far beyond the request cap is cut off as too large.
	tiny := upload.Policy{MaxSize: 8, Allowed: []string{"*/*"}}
	big, ct := multipartBody(t, []formFile{{"file", "a.bin", strings.Repeat("x", 200<<10)}}, nil)
	rec = post(t, upload.Handler(d, tiny, upload.HandlerOptions{}), big, ct)
	if rec.Code != http.StatusRequestEntityTooLarge || errorBody(t, rec).Code != filesystem.CodeTooLarge {
		t.Errorf("oversize request = %d %s", rec.Code, rec.Body)
	}
	// Junk fields large enough to pass the per-file cap still hit the
	// request cap.
	junk, ct := multipartBody(t, nil, map[string]string{"junk": strings.Repeat("j", 200<<10)})
	if rec := post(t, upload.Handler(d, tiny, upload.HandlerOptions{}), junk, ct); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize form fields = %d", rec.Code)
	}
}

// The request cap tripping inside a write must not show the client the
// Disk's annotated error, whose text names the key being written.
func TestHandlerTooLargeHidesKey(t *testing.T) {
	d := newDisk(t, memory.New())
	p := upload.Policy{MaxSize: 100 << 10, Allowed: []string{"*/*"}}
	// The junk field and the file each pass their own cap; together they
	// pass the request cap while the file is being written.
	body, ct := multipartBody(t, []formFile{{"file", "a.bin", strings.Repeat("x", 99<<10)}}, map[string]string{"junk": strings.Repeat("j", 70<<10)})
	rec := post(t, upload.Handler(d, p, upload.HandlerOptions{}), body, ct)
	e := errorBody(t, rec)
	if rec.Code != http.StatusRequestEntityTooLarge || e.Code != filesystem.CodeTooLarge {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(e.Error, "uploads:") || strings.Contains(e.Error, "write") {
		t.Errorf("message leaks the write: %q", e.Error)
	}
	if n := count(t, d); n != 0 {
		t.Errorf("%d objects stored", n)
	}
}

// A client-visible error that wraps an annotated *filesystem.Error shows
// only the inner error's own client-facing words, never its key.
func TestHandlerNestedErrorHidesKey(t *testing.T) {
	d := newDisk(t, memory.New())
	inner := &filesystem.Error{Code: filesystem.CodeNotFound, Op: filesystem.OpRead, Disk: "uploads", Key: "secret/key.png", Err: errors.New("gone")}
	for name, tc := range map[string]struct {
		wrapped error
		want    string
	}{
		"hidden inner code":  {inner, http.StatusText(http.StatusUnsupportedMediaType)},
		"visible inner code": {filesystem.Errorf(filesystem.CodeRejected, "watermark failed"), "watermark failed"},
	} {
		t.Run(name, func(t *testing.T) {
			p := images
			p.Process = func(context.Context, upload.Info, io.Reader) (io.Reader, upload.Info, error) {
				return nil, upload.Info{}, filesystem.NewError(filesystem.CodeRejected, tc.wrapped)
			}
			body, ct := multipartBody(t, []formFile{{"file", "a.png", pngData}}, nil)
			rec := post(t, upload.Handler(d, p, upload.HandlerOptions{}), body, ct)
			if got := errorBody(t, rec).Error; got != tc.want {
				t.Errorf("message %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHandlerPassesRetryAfter(t *testing.T) {
	d := newDisk(t, memory.New())
	p := images
	p.Process = func(context.Context, upload.Info, io.Reader) (io.Reader, upload.Info, error) {
		return nil, upload.Info{}, filesystem.NewError(filesystem.CodeUnavailable, errors.New("throttled")).WithRetryAfter(3 * time.Second)
	}
	body, ct := multipartBody(t, []formFile{{"file", "a.png", pngData}}, nil)
	rec := post(t, upload.Handler(d, p, upload.HandlerOptions{}), body, ct)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "3" {
		t.Errorf("got %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}
