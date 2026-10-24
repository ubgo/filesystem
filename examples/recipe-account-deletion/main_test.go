package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
)

type env struct {
	app  *App
	base string
}

func setup(t *testing.T, opts ...filesystem.Option) env {
	t.Helper()
	app, err := New(t.Context(), t.TempDir(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Handler)
	t.Cleanup(srv.Close)
	return env{app: app, base: srv.URL}
}

func (e env) do(t *testing.T, method, path, contentType string, body io.Reader) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.base+path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func (e env) user(t *testing.T, id string) {
	t.Helper()
	if status, body := e.do(t, http.MethodPost, "/users/"+id, "application/json", strings.NewReader(`{"email":"`+id+`@example.com"}`)); status != http.StatusCreated {
		t.Fatalf("create user = %d %s", status, body)
	}
}

func (e env) upload(t *testing.T, user, name string) fileResponse {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = io.WriteString(fw, "contents of "+name)
	_ = mw.Close()
	status, data := e.do(t, http.MethodPost, "/users/"+user+"/files", mw.FormDataContentType(), &body)
	var f fileResponse
	if err := json.Unmarshal([]byte(data), &f); err != nil || status != http.StatusCreated {
		t.Fatalf("upload = %d %s", status, data)
	}
	return f
}

// objects counts the objects on the disk.
func (e env) objects(t *testing.T) int {
	t.Helper()
	disk, _ := e.app.Disks.Disk(diskName)
	n := 0
	for _, err := range disk.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	return n
}

func TestDeleteAccountKeepsSharedFiles(t *testing.T) {
	e := setup(t)
	e.user(t, "u1")
	e.upload(t, "u1", "a.txt")
	e.upload(t, "u1", "b.txt")
	shared := e.upload(t, "u1", "plan.txt")
	if status, _ := e.do(t, http.MethodPut, "/teams/t1/files/"+shared.ID, "", nil); status != http.StatusNoContent {
		t.Fatalf("share = %d", status)
	}
	status, body := e.do(t, http.MethodDelete, "/users/u1", "", nil)
	if status != http.StatusOK || !strings.Contains(body, `"files_purged":2`) {
		t.Fatalf("delete = %d %s", status, body)
	}
	if _, body := e.do(t, http.MethodGet, "/teams/t1/files", "", nil); !strings.Contains(body, shared.ID) {
		t.Errorf("team lost the shared file: %s", body)
	}
	if n := e.objects(t); n != 1 {
		t.Errorf("%d objects left, want only the shared one", n)
	}
	var users int
	_ = e.app.DB.QueryRow("SELECT COUNT(*) FROM users").Scan(&users)
	if users != 0 {
		t.Errorf("user row survived")
	}
}

func TestFailedDeletionRollsBack(t *testing.T) {
	e := setup(t)
	e.user(t, "u2")
	e.upload(t, "u2", "a.txt")
	e.do(t, http.MethodPost, "/users/u2/invoices", "", nil)
	if status, _ := e.do(t, http.MethodDelete, "/users/u2", "", nil); status != http.StatusConflict {
		t.Fatalf("delete with invoices = %d, want 409", status)
	}
	if _, body := e.do(t, http.MethodGet, "/users/u2/files", "", nil); !strings.Contains(body, "a.txt") {
		t.Errorf("rolled-back deletion lost the file: %s", body)
	}
	if n := e.objects(t); n != 1 {
		t.Errorf("rolled-back deletion deleted objects: %d left", n)
	}
}

func TestObjectFailureIsQueuedAndRetried(t *testing.T) {
	var failDeletes atomic.Bool
	fail := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpDelete && failDeletes.Load() {
			return filesystem.Errorf(filesystem.CodeUnavailable, "backend down")
		}
		return call(ctx)
	}
	e := setup(t, filesystem.WithInterceptors(fail))
	e.user(t, "u3")
	e.upload(t, "u3", "a.txt")
	failDeletes.Store(true)
	if status, body := e.do(t, http.MethodDelete, "/users/u3", "", nil); status != http.StatusOK {
		t.Fatalf("delete = %d %s (rows must go even when the object cannot)", status, body)
	}
	var queued int
	_ = e.app.DB.QueryRow("SELECT COUNT(*) FROM orphaned_objects").Scan(&queued)
	if queued != 1 || e.objects(t) != 1 {
		t.Fatalf("queued %d, objects %d", queued, e.objects(t))
	}
	failDeletes.Store(false)
	if err := e.app.maintain(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = e.app.DB.QueryRow("SELECT COUNT(*) FROM orphaned_objects").Scan(&queued)
	if queued != 0 || e.objects(t) != 0 {
		t.Errorf("after retry: queued %d, objects %d", queued, e.objects(t))
	}
}

func TestTrashRetention(t *testing.T) {
	e := setup(t)
	e.user(t, "u4")
	f := e.upload(t, "u4", "a.txt")
	if status, _ := e.do(t, http.MethodDelete, "/users/u4/files/"+f.ID, "", nil); status != http.StatusNoContent {
		t.Fatalf("trash = %d", status)
	}
	if err := e.app.maintain(t.Context(), time.Now()); err != nil || e.objects(t) != 1 {
		t.Fatalf("a fresh trash item was purged: %v", err)
	}
	if err := e.app.maintain(t.Context(), time.Now().Add(trashRetention+time.Hour)); err != nil || e.objects(t) != 0 {
		t.Errorf("an expired trash item survived: %v", err)
	}
}
