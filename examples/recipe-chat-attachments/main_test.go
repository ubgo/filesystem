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

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordshttp"
)

const pdf = "%PDF-1.4\n%%EOF\n"

type env struct {
	t    *testing.T
	app  *App
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
	if err := app.Seed(t.Context(), map[string][]string{"general": {"alice", "bob"}, "leads": {"carol"}}); err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = app.Handler
	return &env{t: t, app: app, base: srv.URL}
}

// do sends a request as user and returns the status and body.
func (e *env) do(method, path, user, contentType string, body io.Reader) (int, []byte) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.base+path, body)
	req.Header.Set(headerUser, user)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// attach uploads files (name → content) to a message as user.
func (e *env) attach(msg, user string, files ...[2]string) (int, recordshttp.ListResponse) {
	e.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range files {
		w, _ := mw.CreateFormFile("file", f[0])
		_, _ = io.WriteString(w, f[1])
	}
	_ = mw.Close()
	code, data := e.do(http.MethodPost, "/media/owners/message/"+msg+"/attachments", user, mw.FormDataContentType(), &body)
	var out recordshttp.ListResponse
	_ = json.Unmarshal(data, &out)
	return code, out
}

func (e *env) post(conv, user string) string {
	e.t.Helper()
	code, data := e.do(http.MethodPost, "/conversations/"+conv+"/messages", user, "application/json", strings.NewReader(`{"body":"hi"}`))
	var out messageResponse
	if err := json.Unmarshal(data, &out); err != nil || code != http.StatusCreated {
		e.t.Fatalf("post message = %d %s", code, data)
	}
	return out.ID
}

func (e *env) files() int {
	e.t.Helper()
	page, err := e.app.Store.Files(e.t.Context(), records.FileQuery{Scope: workspace})
	if err != nil {
		e.t.Fatal(err)
	}
	return len(page.Files)
}

func TestMembersOnly(t *testing.T) {
	e := setup(t)
	msg := e.post("general", "alice")
	if code, _ := e.do(http.MethodPost, "/conversations/general/messages", "carol", "application/json", strings.NewReader(`{}`)); code != http.StatusForbidden {
		t.Errorf("non-member posting = %d", code)
	}
	code, list := e.attach(msg, "alice", [2]string{"Angebot Müller.pdf", pdf}, [2]string{"notes.txt", "hello"})
	if code != http.StatusCreated || len(list.Files) != 2 {
		t.Fatalf("attach = %d %+v", code, list)
	}
	if code, _ := e.attach(msg, "bob", [2]string{"x.txt", "x"}); code != http.StatusForbidden {
		t.Errorf("non-author attaching = %d", code)
	}
	if code, _ := e.do(http.MethodGet, "/media/owners/message/"+msg+"/attachments", "bob", "", nil); code != http.StatusOK {
		t.Errorf("member listing = %d", code)
	}
	fileID := list.Files[0].FileID
	for user, want := range map[string]int{"bob": http.StatusFound, "carol": http.StatusForbidden} {
		if code, _ := e.do(http.MethodGet, "/media/files/"+fileID, user, "", nil); code != want {
			t.Errorf("%s reading = %d, want %d", user, code, want)
		}
		if code, _ := e.do(http.MethodGet, "/downloads/"+fileID, user, "", nil); code != want {
			t.Errorf("%s downloading = %d, want %d", user, code, want)
		}
	}
	if code, _ := e.do(http.MethodDelete, "/media/files/"+fileID, "alice", "", nil); code != http.StatusForbidden {
		t.Errorf("trashing one file = %d", code)
	}
}

func TestAllOrNothing(t *testing.T) {
	e := setup(t)
	msg := e.post("general", "alice")
	code, _ := e.attach(msg, "alice", [2]string{"ok.txt", "fine"}, [2]string{"page.html", "<html><script>x</script>"})
	if code != http.StatusUnsupportedMediaType || e.files() != 0 {
		t.Errorf("mixed upload = %d with %d files stored", code, e.files())
	}
}

func TestDeleteMessage(t *testing.T) {
	e := setup(t)
	msg := e.post("general", "alice")
	if code, _ := e.attach(msg, "alice", [2]string{"a.txt", "a"}, [2]string{"b.txt", "b"}); code != http.StatusCreated {
		t.Fatal(code)
	}
	// A failing message delete rolls the purge back with it.
	if _, err := e.app.DB.Exec(`CREATE TRIGGER block BEFORE DELETE ON messages BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.do(http.MethodDelete, "/messages/"+msg, "alice", "", nil); code != http.StatusInternalServerError || e.files() != 2 {
		t.Fatalf("failed delete = %d, %d files left", code, e.files())
	}
	_, _ = e.app.DB.Exec(`DROP TRIGGER block`)
	if code, _ := e.do(http.MethodDelete, "/messages/"+msg, "bob", "", nil); code != http.StatusForbidden {
		t.Errorf("non-author delete = %d", code)
	}
	if code, _ := e.do(http.MethodDelete, "/messages/"+msg, "alice", "", nil); code != http.StatusNoContent || e.files() != 0 {
		t.Errorf("delete = %d, %d files left", code, e.files())
	}
	if _, err := e.app.message(t.Context(), msg); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("message row after delete: %v", err)
	}
}
