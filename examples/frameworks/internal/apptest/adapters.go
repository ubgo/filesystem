package apptest

import (
	"archive/zip"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/examples/frameworks/internal/adapterapp"
)

// adapterRole is the records role the adapter walk uploads to and exports.
const adapterRole = "docs"

// ExerciseAdapters walks every route of an adapter example (adapters/gin,
// adapters/echo, adapters/fiber, adapters/chi) served at base: a form
// upload and a refused one, the records endpoints without a user (401) and
// as one (201, uploaded_by set by the adapter's Actor), the signed
// download behind them, the export zip, and an empty export answered 404
// by the adapter's error mapping.
func ExerciseAdapters(t *testing.T, base string) {
	body, ct := Form(t, "report.txt", TextBody)
	if status, out := doAs(t, http.MethodPost, base+adapterapp.PathUpload, "", ct, body); status != http.StatusCreated {
		t.Fatalf("upload = %d %s", status, out)
	}
	body, ct = Form(t, "page.html", htmlBody)
	if status, out := doAs(t, http.MethodPost, base+adapterapp.PathUpload, "", ct, body); status != http.StatusUnsupportedMediaType || !strings.Contains(out, `"code":"rejected"`) {
		t.Errorf("html upload = %d %s", status, out)
	}
	owner := base + adapterapp.GroupMe + adapterapp.MountMedia + "/owners/user/alice/" + adapterRole
	body, ct = Form(t, "notes.txt", TextBody)
	if status, out := doAs(t, http.MethodPost, owner, "", ct, body); status != http.StatusUnauthorized {
		t.Errorf("anonymous attach = %d %s", status, out)
	}
	body, ct = Form(t, "notes.txt", TextBody)
	status, out := doAs(t, http.MethodPost, owner, "alice", ct, body)
	var attached struct {
		Files []struct {
			FileID     string `json:"file_id"`
			UploadedBy string `json:"uploaded_by"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &attached); err != nil || status != http.StatusCreated || len(attached.Files) != 1 || attached.Files[0].UploadedBy != "alice" {
		t.Fatalf("attach = %d %s", status, out)
	}
	status, got := doAs(t, http.MethodGet, base+adapterapp.GroupMe+adapterapp.MountMedia+"/files/"+attached.Files[0].FileID, "alice", "", nil)
	if status != http.StatusOK || got != TextBody {
		t.Errorf("download = %d %q", status, got)
	}
	status, got = doAs(t, http.MethodGet, base+adapterapp.GroupMe+adapterapp.PathExport+"/"+adapterRole, "alice", "", nil)
	if status != http.StatusOK {
		t.Fatalf("export = %d %s", status, got)
	}
	zr, err := zip.NewReader(strings.NewReader(got), int64(len(got)))
	if err != nil || len(zr.File) != 1 || zr.File[0].Name != "1-notes.txt" {
		t.Fatalf("export zip: %v", err)
	}
	if status, got := doAs(t, http.MethodGet, base+adapterapp.GroupMe+adapterapp.PathExport+"/photos", "alice", "", nil); status != http.StatusNotFound || !strings.Contains(got, `"code":"not_found"`) {
		t.Errorf("empty export = %d %s", status, got)
	}
}

// doAs sends a request as user (none when "") and, unlike Do, follows
// redirects, so a records download arrives at the signed link's bytes.
func doAs(t *testing.T, method, url, user, contentType string, body io.Reader) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.Header.Set(adapterapp.HeaderUser, user)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}
