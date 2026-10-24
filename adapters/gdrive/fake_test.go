package gdrive_test

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem/adapters/gdrive"
)

const testToken = "ya29.test-drive-token"

// dItem is one Drive file or folder.
type dItem struct {
	id, name, parent, mime string
	data                   []byte
	created                int
	mod                    time.Time
	// trashed is set by a files.update with {"trashed": true}; listings
	// skip trashed items, as the driver's "trashed = false" query asks.
	trashed bool
}

type upload struct {
	target string // file ID for an update, "" for a create
	meta   map[string]any
	data   []byte
}

// fake is an in-process Drive API v3 serving the requests the driver
// makes: files.list with the query forms the driver writes, alt=media with
// Range, folder creation, copy, update with addParents/removeParents,
// delete, and resumable uploads with 308 between chunks. It allows
// repeated names, as Drive does.
type fake struct {
	*httptest.Server
	mu      sync.Mutex
	items   map[string]*dItem
	uploads map[string]*upload
	seq     int
	ranges  []string
	// contentRanges records each upload chunk's Content-Range.
	contentRanges []string
	// intercept, when set, sees every authorised request first and answers
	// it instead of the fake when it returns true, to inject a failure or
	// an odd answer; it runs under mu.
	intercept func(w http.ResponseWriter, r *http.Request) bool
	// hits counts the requests that reached a host other than the fake
	// (set by seeded; nil otherwise).
	hits func() int
}

// on sets the intercept.
func (f *fake) on(h func(w http.ResponseWriter, r *http.Request) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.intercept = h
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{items: map[string]*dItem{}, uploads: map[string]*upload{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fake) apiURL() string    { return f.URL + "/drive/v3" }
func (f *fake) uploadURL() string { return f.URL + "/upload/drive/v3" }

// add stores an item directly, for tests that need repeated names.
func (f *fake) add(name, parent, mime string, data []byte) *dItem {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.create(name, parent, mime, data)
}

func (f *fake) create(name, parent, mime string, data []byte) *dItem {
	f.seq++
	if mime == "" {
		mime = "application/octet-stream"
	}
	it := &dItem{id: fmt.Sprintf("id%04d", f.seq), name: name, parent: parent, mime: mime, data: data, created: f.seq, mod: time.Now()}
	f.items[it.id] = it
	return it
}

func (f *fake) fail(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": status, "message": reason, "errors": []map[string]string{{"reason": reason}}}})
}

func (f *fake) json(it *dItem) map[string]any {
	m := map[string]any{"id": it.id, "name": it.name, "mimeType": it.mime, "modifiedTime": it.mod.UTC().Format(time.RFC3339Nano)}
	if it.mime != gdrive.FolderMIME {
		sum := md5.Sum(it.data)
		m["size"], m["md5Checksum"] = strconv.Itoa(len(it.data)), hex.EncodeToString(sum[:])
	}
	return m
}

var listQuery = regexp.MustCompile(`^'((?:[^'\\]|\\.)*)' in parents and trashed = false(?: and name = '((?:[^'\\]|\\.)*)')?$`)

func unquote(s string) string { return strings.NewReplacer(`\\`, `\`, `\'`, `'`).Replace(s) }

// listPage is small so the driver must follow nextPageToken.
const listPage = 2

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		f.fail(w, http.StatusUnauthorized, "authError")
		return
	}
	if f.intercept != nil && f.intercept(w, r) {
		return
	}
	q := r.URL.Query()
	switch p := r.URL.Path; {
	case p == "/drive/v3/files" && r.Method == http.MethodGet:
		m := listQuery.FindStringSubmatch(q.Get("q"))
		if m == nil {
			f.fail(w, http.StatusBadRequest, "invalidQuery")
			return
		}
		parent, name := unquote(m[1]), unquote(m[2])
		var kids []*dItem
		for _, it := range f.items {
			if !it.trashed && it.parent == parent && (m[2] == "" && !strings.Contains(q.Get("q"), "name =") || it.name == name) {
				kids = append(kids, it)
			}
		}
		sort.Slice(kids, func(i, j int) bool { return kids[i].created < kids[j].created })
		start, _ := strconv.Atoi(q.Get("pageToken"))
		end := min(start+listPage, len(kids))
		files := []map[string]any{}
		for _, it := range kids[start:end] {
			files = append(files, f.json(it))
		}
		out := map[string]any{"files": files}
		if end < len(kids) {
			out["nextPageToken"] = strconv.Itoa(end)
		}
		_ = json.NewEncoder(w).Encode(out)
	case p == "/drive/v3/files" && r.Method == http.MethodPost:
		var in struct {
			Name     string   `json:"name"`
			MimeType string   `json:"mimeType"`
			Parents  []string `json:"parents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		_ = json.NewEncoder(w).Encode(f.json(f.create(in.Name, in.Parents[0], in.MimeType, nil)))
	case strings.HasPrefix(p, "/drive/v3/files/"):
		f.file(w, r, strings.TrimPrefix(p, "/drive/v3/files/"))
	case strings.HasPrefix(p, "/upload/drive/v3/files"):
		f.upload(w, r, strings.TrimPrefix(strings.TrimPrefix(p, "/upload/drive/v3/files"), "/"))
	default:
		http.NotFound(w, r)
	}
}

func (f *fake) file(w http.ResponseWriter, r *http.Request, rest string) {
	id, op, _ := strings.Cut(rest, "/")
	it := f.items[id]
	if it == nil {
		f.fail(w, http.StatusNotFound, "notFound")
		return
	}
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && q.Get("alt") == "media":
		f.ranges = append(f.ranges, r.Header.Get("Range"))
		http.ServeContent(w, r, "", it.mod, bytes.NewReader(it.data))
	case r.Method == http.MethodPost && op == "copy":
		var in struct {
			Name    string   `json:"name"`
			Parents []string `json:"parents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		_ = json.NewEncoder(w).Encode(f.json(f.create(in.Name, in.Parents[0], it.mime, it.data)))
	case r.Method == http.MethodPatch:
		var in struct {
			Name    string `json:"name"`
			Trashed bool   `json:"trashed"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		it.trashed = it.trashed || in.Trashed
		if add := q.Get("addParents"); add != "" {
			if q.Get("removeParents") != it.parent {
				f.fail(w, http.StatusBadRequest, "badParent")
				return
			}
			it.parent = add
		}
		if in.Name != "" {
			it.name = in.Name
		}
		_ = json.NewEncoder(w).Encode(f.json(it))
	case r.Method == http.MethodDelete:
		f.removeTree(id)
		w.WriteHeader(http.StatusNoContent)
	default:
		f.fail(w, http.StatusBadRequest, "unexpected")
	}
}

func (f *fake) removeTree(id string) {
	delete(f.items, id)
	for k, it := range f.items {
		if it.parent == id {
			f.removeTree(k)
		}
	}
}

func (f *fake) upload(w http.ResponseWriter, r *http.Request, id string) {
	q := r.URL.Query()
	if q.Get("uploadType") != "resumable" {
		f.fail(w, http.StatusBadRequest, "uploadType")
		return
	}
	if sid := q.Get("upload_id"); sid != "" {
		f.chunk(w, r, sid)
		return
	}
	if id != "" && f.items[id] == nil {
		f.fail(w, http.StatusNotFound, "notFound")
		return
	}
	var meta map[string]any
	_ = json.NewDecoder(r.Body).Decode(&meta)
	f.seq++
	sid := strconv.Itoa(f.seq)
	f.uploads[sid] = &upload{target: id, meta: meta}
	w.Header().Set("Location", f.URL+"/upload/drive/v3/files?uploadType=resumable&upload_id="+sid)
	w.WriteHeader(http.StatusOK)
}

func (f *fake) chunk(w http.ResponseWriter, r *http.Request, sid string) {
	up := f.uploads[sid]
	if up == nil {
		f.fail(w, http.StatusNotFound, "notFound")
		return
	}
	if r.Method == http.MethodDelete {
		delete(f.uploads, sid)
		w.WriteHeader(499)
		return
	}
	cr := r.Header.Get("Content-Range")
	f.contentRanges = append(f.contentRanges, cr)
	data, _ := io.ReadAll(r.Body)
	var total string
	if t, ok := strings.CutPrefix(cr, "bytes */"); ok {
		total = t
	} else {
		var start, end int
		if _, err := fmt.Sscanf(cr, "bytes %d-%d/%s", &start, &end, &total); err != nil || start != len(up.data) || end-start+1 != len(data) {
			f.fail(w, http.StatusBadRequest, "badContentRange")
			return
		}
	}
	up.data = append(up.data, data...)
	if total == "*" || total != strconv.Itoa(len(up.data)) {
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(up.data)-1))
		w.WriteHeader(308)
		return
	}
	delete(f.uploads, sid)
	mime, _ := up.meta["mimeType"].(string)
	if up.target != "" {
		it := f.items[up.target]
		it.data, it.mod = up.data, time.Now()
		if mime != "" {
			it.mime = mime
		}
		_ = json.NewEncoder(w).Encode(f.json(it))
		return
	}
	name, _ := up.meta["name"].(string)
	parents, _ := up.meta["parents"].([]any)
	parent, _ := parents[0].(string)
	_ = json.NewEncoder(w).Encode(f.json(f.create(name, parent, mime, up.data)))
}
