package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/contrib/imaging/dimensions"
	"github.com/ubgo/filesystem/records/recordshttp"
)

// client calls the drive as one user of one workspace.
type client struct {
	t         *testing.T
	base      string
	workspace string
	user      string
}

func (c client) do(method, path, contentType string, body io.Reader) (*http.Response, []byte) {
	c.t.Helper()
	req, _ := http.NewRequestWithContext(c.t.Context(), method, c.base+path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.workspace != "" {
		req.Header.Set(headerSpace, c.workspace)
	}
	req.Header.Set(headerUser, c.user)
	resp, err := http.DefaultTransport.RoundTrip(req) // no redirect following
	if err != nil {
		c.t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, data
}

// json sends v as JSON and decodes the answer into out, requiring want.
func (c client) json(method, path string, v, out any, want int) {
	c.t.Helper()
	var body io.Reader
	if v != nil {
		data, _ := json.Marshal(v)
		body = bytes.NewReader(data)
	}
	resp, data := c.do(method, path, "application/json", body)
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s = %d %s, want %d", method, path, resp.StatusCode, data, want)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			c.t.Fatalf("%s %s: %v in %s", method, path, err, data)
		}
	}
}

func (c client) form(method, path, name string, content []byte, want int, out any) {
	c.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	w, _ := mw.CreateFormFile(recordshttp.FormField, name)
	_, _ = w.Write(content)
	_ = mw.Close()
	resp, data := c.do(method, path, mw.FormDataContentType(), &body)
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s = %d %s, want %d", method, path, resp.StatusCode, data, want)
	}
	if out != nil {
		_ = json.Unmarshal(data, out)
	}
}

func pngOf(w, h int) []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, w, h)))
	return buf.Bytes()
}

// TestDrive walks one workspace through the life of a file: upload,
// folder, tags, a new version, a restore, a share link, the activity feed
// and the usage, and checks another workspace sees none of it.
func TestDrive(t *testing.T) {
	srv := httptest.NewServer(nil)
	defer srv.Close()
	app, err := New(t.Context(), t.TempDir(), srv.URL, bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = app.Handler
	ana := client{t: t, base: srv.URL, workspace: "acme", user: "ana"}
	ben := client{t: t, base: srv.URL, workspace: "acme", user: "ben"}
	eve := client{t: t, base: srv.URL, workspace: "evil-corp", user: "eve"}

	var up recordshttp.ListResponse
	ana.form(http.MethodPost, "/api/owners/project/p1/assets", "logo.png", pngOf(64, 32), http.StatusCreated, &up)
	logo := up.Files[0]

	var docs folderJSON
	ana.json(http.MethodPost, "/api/folders", folderRequest{Name: "Brand"}, &docs, http.StatusOK)
	ana.json(http.MethodPut, "/api/files/"+logo.FileID+"/folder", moveRequest{Folder: docs.ID}, nil, http.StatusOK)
	var tagged tagsRequest
	ana.json(http.MethodPut, "/api/files/"+logo.FileID+"/tags", tagsRequest{Tags: []string{"Logo", "brand"}}, &tagged, http.StatusOK)
	if strings.Join(tagged.Tags, ",") != "brand,logo" {
		t.Fatalf("tags = %v", tagged.Tags)
	}

	// Ben uploads a new version; the old one is kept and the dimensions follow.
	ben.form(http.MethodPut, "/api/files/"+logo.FileID, "logo-v2.png", pngOf(128, 128), http.StatusOK, nil)
	f, _ := app.Store.File(t.Context(), "acme", logo.FileID)
	if w, h, ok := dimensions.Of(f); !ok || w != 128 || h != 128 {
		t.Fatalf("dimensions after the new version = %d×%d %v", w, h, ok)
	}
	if in, _ := app.Folders.FolderOf(t.Context(), app.Store, "acme", logo.FileID); in != docs.ID {
		t.Fatal("the new version left its folder")
	}
	var history []versionJSON
	ana.json(http.MethodGet, "/api/files/"+logo.FileID+"/versions", nil, &history, http.StatusOK)
	if len(history) != 1 || history[0].Filename != "logo.png" {
		t.Fatalf("history = %+v", history)
	}
	var restored fileJSON
	ana.json(http.MethodPost, "/api/versions/"+history[0].ID+"/restore", nil, &restored, http.StatusOK)
	if restored.Width != 64 || restored.Height != 32 || restored.Filename != "logo.png" || restored.FileID != logo.FileID {
		t.Fatalf("restored = %+v", restored)
	}

	// A share link works for anyone, twice, then never again.
	var link shareResponse
	ana.json(http.MethodPost, "/api/files/"+logo.FileID+"/share", shareRequest{TTLSeconds: 3600, MaxDownloads: 2}, &link, http.StatusOK)
	anyone := client{t: t, base: srv.URL}
	for range 2 {
		resp, _ := anyone.do(http.MethodGet, link.Path, "", nil)
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("share link = %d", resp.StatusCode)
		}
		got, body := anyone.do(http.MethodGet, strings.TrimPrefix(resp.Header.Get("Location"), srv.URL), "", nil)
		if got.StatusCode != http.StatusOK || !bytes.Equal(body, pngOf(64, 32)) {
			t.Fatalf("download = %d, %d bytes", got.StatusCode, len(body))
		}
	}
	if resp, _ := anyone.do(http.MethodGet, link.Path, "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("spent share link = %d", resp.StatusCode)
	}
	ana.json(http.MethodPost, "/api/files/"+logo.FileID+"/share", shareRequest{TTLSeconds: 0}, nil, http.StatusBadRequest)

	// The feed says who did what, newest first, including the plugins'
	// events (folders, tags, share links); a download through a share link
	// has no actor.
	var feed activityJSON
	ana.json(http.MethodGet, "/api/activity", nil, &feed, http.StatusOK)
	var lines []string
	for _, e := range feed.Entries {
		lines = append(lines, e.Actor+" "+e.Kind)
	}
	if got := strings.Join(lines, "; "); got != " share.downloaded;  share.downloaded; user:ana share.created; user:ana file.replaced; user:ben file.replaced; user:ana tags.added; user:ana tags.added; user:ana folders.file_put; user:ana folders.created; user:ana attachment.created; user:ana file.created" {
		t.Fatalf("activity = %s", got)
	}

	// Usage counts the current content of each file.
	var used usageJSON
	ana.json(http.MethodGet, "/api/usage", nil, &used, http.StatusOK)
	if used.Files != 1 || used.Bytes != int64(len(pngOf(64, 32))) {
		t.Fatalf("usage = %+v", used)
	}

	// Another workspace sees nothing and changes nothing.
	eve.json(http.MethodGet, "/api/files/"+logo.FileID+"/versions", nil, nil, http.StatusNotFound)
	eve.json(http.MethodPut, "/api/files/"+logo.FileID+"/tags", tagsRequest{Tags: []string{"pwned"}}, nil, http.StatusNotFound)
	eve.json(http.MethodPost, "/api/files/"+logo.FileID+"/share", shareRequest{TTLSeconds: 60}, nil, http.StatusNotFound)
	if resp, _ := eve.do(http.MethodGet, "/api/files/"+logo.FileID, "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another workspace reading the file = %d", resp.StatusCode)
	}
	var theirs activityJSON
	eve.json(http.MethodGet, "/api/activity", nil, &theirs, http.StatusOK)
	if len(theirs.Entries) != 0 {
		t.Fatalf("another workspace's feed = %+v", theirs.Entries)
	}

	// Without a workspace nothing works.
	nobody := client{t: t, base: srv.URL, user: "x"}
	nobody.json(http.MethodGet, "/api/usage", nil, nil, http.StatusUnauthorized)
	ana.json(http.MethodPost, "/api/folders", "not an object", nil, http.StatusBadRequest)
}
