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
)

type env struct {
	t    *testing.T
	base string
}

func setup(t *testing.T) *env {
	t.Helper()
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	app, err := New(t.Context(), t.TempDir(), srv.URL, bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = app.Handler
	return &env{t: t, base: srv.URL}
}

// call sends a request in workspace ws as user and decodes a JSON answer
// into out (when not nil).
func call[T any](e *env, method, path, ws, contentType string, body io.Reader, out *T) int {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.base+path, body)
	req.Header.Set(headerWorkspace, ws)
	req.Header.Set(headerUser, "alice")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
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

// send uploads content as name with method to path in ws.
func (e *env) send(method, path, ws, name, content string) (int, fileResponse) {
	e.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	w, _ := mw.CreateFormFile("file", name)
	_, _ = io.WriteString(w, content)
	_ = mw.Close()
	var out fileResponse
	code := call(e, method, path, ws, mw.FormDataContentType(), &body, &out)
	return code, out
}

func TestFoldersVersionsShares(t *testing.T) {
	e := setup(t)
	var folder folderResponse
	if code := call(e, http.MethodPost, "/folders", "studio", "", strings.NewReader(`{"name":"Contracts"}`), &folder); code != http.StatusOK {
		t.Fatalf("create folder = %d", code)
	}
	code, f := e.send(http.MethodPost, "/files?folder="+folder.ID, "studio", "offer.txt", "v1")
	if code != http.StatusCreated {
		t.Fatalf("upload = %d", code)
	}
	var listed []fileResponse
	call(e, http.MethodGet, "/folders/"+folder.ID+"/files", "studio", "", nil, &listed)
	if len(listed) != 1 || listed[0].ID != f.ID {
		t.Errorf("folder listing = %+v", listed)
	}
	if code := call[none](e, http.MethodGet, "/folders/"+folder.ID+"/files", "other", "", nil, nil); code != http.StatusNotFound {
		t.Errorf("other workspace listing = %d", code)
	}
	if code, _ := e.send(http.MethodPut, "/files/"+f.ID, "studio", "offer.txt", "v2, longer"); code != http.StatusOK {
		t.Fatalf("replace = %d", code)
	}
	var vs []versionResponse
	call(e, http.MethodGet, "/files/"+f.ID+"/versions", "studio", "", nil, &vs)
	if len(vs) != 1 || vs[0].Size != 2 {
		t.Fatalf("versions = %+v", vs)
	}
	var restored fileResponse
	if code := call(e, http.MethodPost, "/versions/"+vs[0].ID+"/restore", "studio", "", nil, &restored); code != http.StatusOK || restored.Size != 2 {
		t.Errorf("restore = %d %+v", code, restored)
	}
	var sh shareResponse
	call(e, http.MethodPost, "/files/"+f.ID+"/share", "studio", "", strings.NewReader(`{"ttl_seconds":60,"max_downloads":1}`), &sh)
	token := sh.URL[strings.LastIndex(sh.URL, "/"):]
	resp, err := http.Get(e.base + "/s" + token)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(got) != "v1" {
		t.Errorf("share link served %q", got)
	}
	if resp, _ := http.Get(e.base + "/s" + token); resp.StatusCode != http.StatusNotFound {
		t.Errorf("spent share link = %d", resp.StatusCode)
	}
	if code := call[none](e, http.MethodPost, "/files/"+f.ID+"/share", "studio", "", strings.NewReader(`{"ttl_seconds":0}`), nil); code != http.StatusBadRequest {
		t.Errorf("share without ttl = %d", code)
	}
	// The feed carries the plugins' events with what they are about.
	var feed []activityEntry
	call(e, http.MethodGet, "/activity", "studio", "", nil, &feed)
	var created *activityEntry
	for i := range feed {
		if feed[i].Kind == "folders.created" {
			created = &feed[i]
		}
	}
	if created == nil || created.Subject != folder.ID || created.Detail["name"] != "Contracts" {
		t.Errorf("folders.created in the feed = %+v", created)
	}
}

func TestQuota(t *testing.T) {
	e := setup(t)
	half := strings.Repeat("a", int(plans["free"].Bytes)/2+1)
	if code, _ := e.send(http.MethodPost, "/files", "studio", "a.txt", half); code != http.StatusCreated {
		t.Fatalf("first = %d", code)
	}
	if code, _ := e.send(http.MethodPost, "/files", "studio", "b.txt", half); code != http.StatusInsufficientStorage {
		t.Errorf("over quota = %d", code)
	}
	var u usageResponse
	call(e, http.MethodGet, "/usage", "studio", "", nil, &u)
	if u.UsedFiles != 1 || u.UsedBytes != int64(len(half)) {
		t.Errorf("usage after a refused upload = %+v", u)
	}
	if code, _ := e.send(http.MethodPost, "/files", "acme", "b.txt", half+half); code != http.StatusCreated {
		t.Errorf("team plan = %d", code)
	}
	var feed []activityEntry
	call(e, http.MethodGet, "/activity", "studio", "", nil, &feed)
	if len(feed) != 1 || feed[0].Actor != "user:alice" {
		t.Errorf("activity = %+v", feed)
	}
}
