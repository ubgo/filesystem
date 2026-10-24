package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem/upload"
)

const jpegData = "\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01-a-direct-upload"

type env struct {
	t    *testing.T
	app  *App
	base string
}

func setup(t *testing.T) *env {
	t.Helper()
	disk, closeDisk, err := openFakeS3(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeDisk)
	app, err := New(t.Context(), disk, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Handler)
	t.Cleanup(srv.Close)
	return &env{t: t, app: app, base: srv.URL}
}

// call sends a request as user and decodes a JSON answer into out (when
// out is not nil), returning the status.
func call[T any](e *env, method, url, user, body string, out *T) int {
	e.t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if user != "" {
		req.Header.Set(headerUser, user)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// put sends data to a presigned URL the way a browser does.
func put(t *testing.T, url, contentType string, data []byte) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func confirmBody(key, name string) string {
	b, _ := json.Marshal(confirmRequest{Key: key, Filename: name})
	return string(b)
}

func TestPresignedFlow(t *testing.T) {
	e := setup(t)
	var tk upload.Ticket
	body := `{"filename":"me.jpg","content_type":"image/jpeg","size":` + strconv.Itoa(len(jpegData)) + `}`
	if code := call(e, "POST", e.base+"/uploads/tickets", "u1", body, &tk); code != 200 || !strings.HasPrefix(tk.Key, "pending/u1/") {
		t.Fatalf("ticket = %d %+v", code, tk)
	}
	if code := put(t, tk.URL, "image/jpeg", []byte(jpegData)); code != 200 {
		t.Fatalf("PUT = %d", code)
	}
	var none upload.ErrorResponse
	if code := call(e, "POST", e.base+"/uploads/confirm", "u2", confirmBody(tk.Key, "me.jpg"), &none); code != http.StatusNotFound {
		t.Errorf("another user's confirm = %d", code)
	}
	var f fileResponse
	if code := call(e, "POST", e.base+"/uploads/confirm", "u1", confirmBody(tk.Key, "me.jpg"), &f); code != http.StatusCreated || f.ContentType != "image/jpeg" || !strings.HasPrefix(f.Key, "files/u1/") {
		t.Fatalf("confirm = %d %+v", code, f)
	}
	var list []fileResponse
	if call(e, "GET", e.base+"/uploads", "u1", "", &list); len(list) != 1 {
		t.Errorf("list = %+v", list)
	}
}

func TestConfirmDeletesWhatThePolicyRefuses(t *testing.T) {
	e := setup(t)
	var tk upload.Ticket
	call(e, "POST", e.base+"/uploads/tickets", "u1", `{"filename":"cat.jpg","content_type":"image/jpeg","size":40}`, &tk)
	put(t, tk.URL, "image/jpeg", []byte("<html><script>alert(1)</script></html>"))
	var er upload.ErrorResponse
	if code := call(e, "POST", e.base+"/uploads/confirm", "u1", confirmBody(tk.Key, "cat.jpg"), &er); code != http.StatusUnsupportedMediaType {
		t.Fatalf("confirm of HTML = %d %+v", code, er)
	}
	if ok, _ := e.app.Disk.Exists(t.Context(), tk.Key); ok {
		t.Error("refused object still stored")
	}
	if code := call(e, "POST", e.base+"/uploads/tickets", "u1", `{"filename":"a.html","content_type":"text/html","size":4}`, &er); code != http.StatusUnsupportedMediaType {
		t.Errorf("declared HTML ticket = %d", code)
	}
	if code := call(e, "POST", e.base+"/uploads/tickets", "u1", `{"filename":"a.mp4","content_type":"video/mp4","size":999999999}`, &er); code != http.StatusRequestEntityTooLarge {
		t.Errorf("declared oversize ticket = %d", code)
	}
}

// uppyCreated mirrors the create answer of the Uppy protocol.
type uppyCreated struct {
	UploadID string `json:"uploadId"`
	Key      string `json:"key"`
}

// uppySigned mirrors the sign-part answer.
type uppySigned struct {
	URL string `json:"url"`
}

func TestUppyMultipart(t *testing.T) {
	e := setup(t)
	var up uppyCreated
	if code := call(e, "POST", e.base+"/uploads/s3/multipart", "u1", `{"filename":"big.jpg","type":"image/jpeg"}`, &up); code != 200 {
		t.Fatalf("create = %d", code)
	}
	part := e.base + "/uploads/s3/multipart/" + up.UploadID + "/1?key=" + up.Key
	var none upload.ErrorResponse
	if code := call(e, "GET", part, "u2", "", &none); code != http.StatusForbidden {
		t.Errorf("another user's sign = %d", code)
	}
	var signed uppySigned
	call(e, "GET", part, "u1", "", &signed)
	req, _ := http.NewRequest(http.MethodPut, signed.URL, strings.NewReader(jpegData))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	etag := resp.Header.Get("ETag")
	complete := `{"parts":[{"PartNumber":1,"ETag":` + strconv.Quote(etag) + `}]}`
	if code := call(e, "POST", e.base+"/uploads/s3/multipart/"+up.UploadID+"/complete?key="+up.Key, "u1", complete, &none); code != 200 {
		t.Fatalf("complete = %d %+v", code, none)
	}
	var f fileResponse
	if code := call(e, "POST", e.base+"/uploads/confirm", "u1", confirmBody(up.Key, "big.jpg"), &f); code != http.StatusCreated {
		t.Fatalf("confirm = %d", code)
	}
}

func TestSweepPending(t *testing.T) {
	e := setup(t)
	var tk upload.Ticket
	call(e, "POST", e.base+"/uploads/tickets", "u1", `{"filename":"a.jpg","content_type":"image/jpeg","size":30}`, &tk)
	put(t, tk.URL, "image/jpeg", []byte(jpegData))
	if n, err := sweepPending(t.Context(), e.app.Disk, time.Now()); n != 0 || err != nil {
		t.Fatalf("fresh sweep = %d %v", n, err)
	}
	if n, err := sweepPending(t.Context(), e.app.Disk, time.Now().Add(pendingMaxAge+time.Hour)); n != 1 || err != nil {
		t.Fatalf("late sweep = %d %v", n, err)
	}
}

// TestUppyRecordsOnComplete completes an Uppy upload on the recorded
// routes with a filename beside the parts: the complete request records
// the file under that name, with no confirm call.
func TestUppyRecordsOnComplete(t *testing.T) {
	e := setup(t)
	base := e.base + recordedPrefix + "/multipart"
	var up uppyCreated
	if code := call(e, "POST", base, "u1", `{"filename":"big.jpg","type":"image/jpeg"}`, &up); code != 200 {
		t.Fatalf("create = %d", code)
	}
	if !strings.HasPrefix(up.Key, filesDir+"/u1/") {
		t.Fatalf("key %q is not under files/u1/", up.Key)
	}
	var signed uppySigned
	call(e, "GET", base+"/"+up.UploadID+"/1?key="+up.Key, "u1", "", &signed)
	req, _ := http.NewRequest(http.MethodPut, signed.URL, strings.NewReader(jpegData))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	complete := `{"filename":"Holiday photo.jpg","parts":[{"PartNumber":1,"ETag":` + strconv.Quote(resp.Header.Get("ETag")) + `}]}`
	var none upload.ErrorResponse
	if code := call(e, "POST", base+"/"+up.UploadID+"/complete?key="+up.Key, "u1", complete, &none); code != 200 {
		t.Fatalf("complete = %d %+v", code, none)
	}
	var files []fileResponse
	call(e, "GET", e.base+"/uploads", "u1", "", &files)
	if len(files) != 1 || files[0].Key != up.Key || files[0].Filename != "Holiday photo.jpg" {
		t.Fatalf("recorded = %+v, want one file at %s named from the complete request", files, up.Key)
	}
	t.Logf("recorded on complete: key under files/u1/, filename %q, %s", files[0].Filename, files[0].ContentType)
}
