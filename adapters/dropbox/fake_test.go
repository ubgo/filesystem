package dropbox_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem/adapters/dropbox"
)

const testToken = "sl.test-access-token"

// fakeFile is one stored file. Paths are case-insensitive, so files are
// keyed by their lower-cased path and keep the cased one for display.
type fakeFile struct {
	display string
	data    []byte
	rev     string
	mod     time.Time
}

// fake is an in-process Dropbox API v2 serving the endpoints the driver
// uses, with the documented error shapes (409 + error_summary), write
// modes, upload sessions, Range reads and case-insensitive paths.
type fake struct {
	*httptest.Server
	mu       sync.Mutex
	files    map[string]*fakeFile
	sessions map[string][]byte
	seq      int
	// requests counts calls by endpoint; hosts records each request's Host
	// with the Authorization it carried.
	requests map[string]int
	auth     []string
	// headers records every request's headers, in order.
	headers []http.Header
	// ranges records the Range header of each download.
	ranges []string
	// override answers an endpoint in place of the fake's own handling, to
	// inject a failure or an odd answer; it runs under mu.
	override map[string]http.HandlerFunc
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{files: map[string]*fakeFile{}, sessions: map[string][]byte{}, requests: map[string]int{}, override: map[string]http.HandlerFunc{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fake) fail(w http.ResponseWriter, status int, summary string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error_summary": summary, "error": map[string]string{".tag": summary}})
}

func (f *fake) meta(file *fakeFile) map[string]any {
	return map[string]any{".tag": "file", "name": file.display[strings.LastIndex(file.display, "/")+1:], "path_display": file.display, "path_lower": strings.ToLower(file.display), "rev": file.rev, "size": len(file.data), "server_modified": file.mod.UTC().Format(time.RFC3339)}
}

// isFolder reports whether any file lives under p.
func (f *fake) isFolder(p string) bool {
	if p == "" {
		return true
	}
	for k := range f.files {
		if strings.HasPrefix(k, strings.ToLower(p)+"/") {
			return true
		}
	}
	return false
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Host+" "+r.Header.Get("Authorization"))
	f.headers = append(f.headers, r.Header.Clone())
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		f.fail(w, http.StatusUnauthorized, "invalid_access_token/")
		return
	}
	endpoint := strings.TrimPrefix(r.URL.Path, "/2/")
	f.requests[endpoint]++
	if h := f.override[endpoint]; h != nil {
		h(w, r)
		return
	}
	var arg map[string]any
	raw := r.Header.Get("Dropbox-API-Arg")
	for _, c := range raw {
		if c > 127 {
			http.Error(w, "Dropbox-API-Arg must be ASCII", http.StatusBadRequest)
			return
		}
	}
	var body []byte
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &arg)
		body, _ = io.ReadAll(r.Body)
	} else {
		_ = json.NewDecoder(r.Body).Decode(&arg)
	}
	str := func(k string) string { s, _ := arg[k].(string); return s }
	switch endpoint {
	case "files/get_metadata":
		p := str("path")
		if file := f.files[strings.ToLower(p)]; file != nil {
			_ = json.NewEncoder(w).Encode(f.meta(file))
			return
		}
		if p != "" && f.isFolder(p) {
			_ = json.NewEncoder(w).Encode(map[string]any{".tag": "folder", "path_display": p})
			return
		}
		f.fail(w, http.StatusConflict, "path/not_found/")
	case "files/download":
		f.ranges = append(f.ranges, r.Header.Get("Range"))
		file := f.files[strings.ToLower(str("path"))]
		if file == nil {
			f.fail(w, http.StatusConflict, "path/not_found/")
			return
		}
		meta, _ := json.Marshal(f.meta(file))
		w.Header().Set("Dropbox-API-Result", string(meta))
		http.ServeContent(w, r, "", file.mod, strings.NewReader(string(file.data)))
	case "files/upload":
		f.commit(w, arg, body)
	case "files/upload_session/start":
		f.seq++
		id := fmt.Sprintf("session-%d", f.seq)
		f.sessions[id] = body
		_ = json.NewEncoder(w).Encode(map[string]string{"session_id": id})
	case "files/upload_session/append_v2", "files/upload_session/finish":
		c, _ := arg["cursor"].(map[string]any)
		id, _ := c["session_id"].(string)
		off, _ := c["offset"].(float64)
		buf, ok := f.sessions[id]
		if !ok {
			f.fail(w, http.StatusConflict, "lookup_failed/not_found/")
			return
		}
		if int(off) != len(buf) {
			f.fail(w, http.StatusConflict, "lookup_failed/incorrect_offset/")
			return
		}
		f.sessions[id] = append(buf, body...)
		if endpoint == "files/upload_session/finish" {
			commit, _ := arg["commit"].(map[string]any)
			data := f.sessions[id]
			delete(f.sessions, id)
			f.commit(w, commit, data)
			return
		}
		_, _ = w.Write([]byte("null"))
	case "files/delete_v2":
		p := strings.ToLower(str("path"))
		n := 0
		for k := range f.files {
			if k == p || strings.HasPrefix(k, p+"/") {
				delete(f.files, k)
				n++
			}
		}
		if n == 0 {
			f.fail(w, http.StatusConflict, "path_lookup/not_found/")
			return
		}
		_, _ = w.Write([]byte("{}"))
	case "files/copy_v2", "files/move_v2":
		f.relocate(w, endpoint, str("from_path"), str("to_path"))
	case "files/list_folder", "files/list_folder/continue":
		f.list(w, arg)
	default:
		http.NotFound(w, r)
	}
}

// relocate is copy_v2 or move_v2 without autorename: it refuses an
// existing destination (other than a case-only rename of from), as Dropbox
// does. It runs under mu.
func (f *fake) relocate(w http.ResponseWriter, endpoint, fromPath, to string) {
	from := strings.ToLower(fromPath)
	file := f.files[from]
	if file == nil {
		f.fail(w, http.StatusConflict, "from_lookup/not_found/")
		return
	}
	if existing := f.files[strings.ToLower(to)]; existing != nil && strings.ToLower(to) != from {
		f.fail(w, http.StatusConflict, "to/conflict/file/")
		return
	}
	f.seq++
	cp := *file
	cp.display, cp.rev = to, strconv.Itoa(f.seq)
	if endpoint == "files/move_v2" {
		delete(f.files, from)
	}
	f.files[strings.ToLower(to)] = &cp
	_, _ = w.Write([]byte("{}"))
}

// failRelocationsWhen is an override for copy_v2 or move_v2 that answers
// with an insufficient_space conflict when match(from, to) holds (paths
// lower-cased) and relocates normally otherwise, to fail one step of a
// multi-call copy or move.
func (f *fake) failRelocationsWhen(endpoint string, match func(from, to string) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var arg struct {
			From string `json:"from_path"`
			To   string `json:"to_path"`
		}
		_ = json.NewDecoder(r.Body).Decode(&arg)
		if match(strings.ToLower(arg.From), strings.ToLower(arg.To)) {
			f.fail(w, http.StatusConflict, "insufficient_space/")
			return
		}
		f.relocate(w, endpoint, arg.From, arg.To)
	}
}

// swapFiles lists the stored paths holding a transient swap file.
func (f *fake) swapFiles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for p := range f.files {
		if strings.Contains(p, strings.ToLower(dropbox.SwapPrefix)) {
			out = append(out, p)
		}
	}
	return out
}

// listPage is the fake's page size, small so the driver must follow
// cursors.
const listPage = 3

func (f *fake) list(w http.ResponseWriter, arg map[string]any) {
	p, _ := arg["path"].(string)
	start := 0
	if c, ok := arg["cursor"].(string); ok {
		parts := strings.SplitN(c, "|", 2)
		start, _ = strconv.Atoi(parts[0])
		p = parts[1]
	} else {
		if f.files[strings.ToLower(p)] != nil {
			f.fail(w, http.StatusConflict, "path/not_folder/")
			return
		}
		if !f.isFolder(p) {
			f.fail(w, http.StatusConflict, "path/not_found/")
			return
		}
	}
	var keys []string
	for k := range f.files {
		if p == "" || strings.HasPrefix(k, strings.ToLower(p)+"/") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	end := min(start+listPage, len(keys))
	entries := []map[string]any{{".tag": "folder", "path_display": p + "/sub-folder-entry"}}
	for _, k := range keys[start:end] {
		entries = append(entries, f.meta(f.files[k]))
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries, "cursor": fmt.Sprintf("%d|%s", end, p), "has_more": end < len(keys)})
}

func (f *fake) commit(w http.ResponseWriter, commit map[string]any, data []byte) {
	p, _ := commit["path"].(string)
	mode, _ := commit["mode"].(map[string]any)
	existing := f.files[strings.ToLower(p)]
	switch mode[".tag"] {
	case "add":
		// Documented: without strict_conflict, an upload identical to the
		// existing file is not a conflict.
		if strict, _ := commit["strict_conflict"].(bool); existing != nil && !strict && string(existing.data) == string(data) {
			_ = json.NewEncoder(w).Encode(f.meta(existing))
			return
		}
		if existing != nil {
			f.fail(w, http.StatusConflict, "path/conflict/file/")
			return
		}
	case "update":
		// Without strict_conflict Dropbox writes a deleted file anew.
		if strict, _ := commit["strict_conflict"].(bool); existing == nil && !strict {
			break
		}
		if existing == nil || existing.rev != mode["update"] {
			f.fail(w, http.StatusConflict, "path/conflict/file/")
			return
		}
	}
	f.seq++
	display := p
	if existing != nil {
		display = existing.display // Dropbox keeps the first casing
	}
	file := &fakeFile{display: display, data: data, rev: fmt.Sprintf("rev%06d", f.seq), mod: time.Now()}
	f.files[strings.ToLower(p)] = file
	_ = json.NewEncoder(w).Encode(f.meta(file))
}

// on makes h answer endpoint from now on.
func (f *fake) on(endpoint string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.override[endpoint] = h
}

// failWith is an override answering with status and summary.
func (f *fake) failWith(status int, summary string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { f.fail(w, status, summary) }
}

// garbage is an override answering 200 with a body that is not JSON.
func garbage(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not json")) }
