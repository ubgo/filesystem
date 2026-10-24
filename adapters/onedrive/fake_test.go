package onedrive_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testToken = "eyJ0eXAi.test-graph-token"
	drivePath = "/v1.0/me/drive/root"
)

// gItem is one driveItem; paths are case-insensitive, so items are keyed
// by their lower-cased path and keep the cased one.
type gItem struct {
	path   string
	folder bool
	data   []byte
	etag   int
	mod    time.Time
}

type session struct {
	path     string
	conflict string
	data     []byte
}

// fake is an in-process Microsoft Graph serving the drive requests the
// driver makes, with the documented shapes: a 302 from /content to a
// pre-authenticated download host, upload sessions on another host,
// conflictBehavior, If-Match, nextLink paging and case-insensitive paths.
// The download and upload hosts fail any request carrying Authorization.
type fake struct {
	t        *testing.T
	graph    *httptest.Server
	files    *httptest.Server // download and upload-session host
	mu       sync.Mutex
	items    map[string]*gItem
	sessions map[string]*session
	seq      int
	// leaked records requests to the files host that carried a token.
	leaked []string
	// ranges and contentRanges record what reads and fragments sent.
	ranges, contentRanges []string
	requests              map[string]int
	// intercept, when set, sees every request (to either host) first and
	// answers it instead of the fake when it returns true, to inject a
	// failure or an odd answer; it runs under mu.
	intercept func(w http.ResponseWriter, r *http.Request) bool
}

// on sets the intercept.
func (f *fake) on(h func(w http.ResponseWriter, r *http.Request) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.intercept = h
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{t: t, items: map[string]*gItem{"": {folder: true}}, sessions: map[string]*session{}, requests: map[string]int{}}
	f.graph = httptest.NewServer(http.HandlerFunc(f.serveGraph))
	f.files = httptest.NewServer(http.HandlerFunc(f.serveFiles))
	t.Cleanup(f.graph.Close)
	t.Cleanup(f.files.Close)
	return f
}

func (f *fake) fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": code}})
}

func (f *fake) json(it *gItem) map[string]any {
	m := map[string]any{"name": path.Base(it.path), "size": len(it.data), "lastModifiedDateTime": it.mod.UTC().Format(time.RFC3339Nano), "eTag": fmt.Sprintf(`"{0000-ETAG},%d"`, it.etag)}
	if it.folder {
		m["folder"] = map[string]any{"childCount": 0}
	} else {
		m["file"] = map[string]any{"mimeType": "application/octet-stream"}
	}
	return m
}

// mkParents creates the folders above p, as Graph does on upload.
func (f *fake) mkParents(p string) bool {
	dir := path.Dir(p)
	if dir == "." {
		return true
	}
	if it := f.items[strings.ToLower(dir)]; it != nil {
		return it.folder
	}
	if !f.mkParents(dir) {
		return false
	}
	f.items[strings.ToLower(dir)] = &gItem{path: dir, folder: true, mod: time.Now()}
	return true
}

func (f *fake) store(p string, data []byte) *gItem {
	f.seq++
	cased := p
	if old := f.items[strings.ToLower(p)]; old != nil {
		cased = old.path
	}
	it := &gItem{path: cased, data: data, etag: f.seq, mod: time.Now()}
	f.items[strings.ToLower(p)] = it
	return it
}

// precondition checks conflictBehavior and If-Match; it writes the error
// and reports false when the write must not happen.
func (f *fake) precondition(w http.ResponseWriter, p, conflict, ifMatch string) bool {
	existing := f.items[strings.ToLower(p)]
	if ifMatch != "" {
		if existing == nil {
			f.fail(w, http.StatusNotFound, "itemNotFound")
			return false
		}
		if strings.Trim(ifMatch, `"`) != strings.Trim(f.json(existing)["eTag"].(string), `"`) {
			f.fail(w, http.StatusPreconditionFailed, "resourceModified")
			return false
		}
	}
	if conflict == "fail" && existing != nil {
		f.fail(w, http.StatusConflict, "nameAlreadyExists")
		return false
	}
	return true
}

func (f *fake) serveGraph(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		f.fail(w, http.StatusUnauthorized, "InvalidAuthenticationToken")
		return
	}
	if f.intercept != nil && f.intercept(w, r) {
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, drivePath)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var p, suffix string
	switch {
	case rest == "" || rest == "/children":
		suffix = rest
	case strings.HasPrefix(rest, ":/"):
		rest = rest[2:]
		i := strings.LastIndex(rest, ":")
		if i < 0 {
			http.Error(w, "missing closing colon", http.StatusBadRequest)
			return
		}
		p, suffix = rest[:i], rest[i+1:]
	default:
		http.NotFound(w, r)
		return
	}
	f.requests[r.Method+" "+suffix]++
	it := f.items[strings.ToLower(p)]
	conflict := r.URL.Query().Get("@microsoft.graph.conflictBehavior")
	switch {
	case r.Method == http.MethodGet && suffix == "":
		if it == nil {
			f.fail(w, http.StatusNotFound, "itemNotFound")
			return
		}
		_ = json.NewEncoder(w).Encode(f.json(it))
	case r.Method == http.MethodGet && suffix == "/content":
		if it == nil || it.folder {
			f.fail(w, http.StatusNotFound, "itemNotFound")
			return
		}
		f.ranges = append(f.ranges, r.Header.Get("Range"))
		http.Redirect(w, r, f.files.URL+"/download/"+strconv.Itoa(it.etag)+"?p="+url.QueryEscape(strings.ToLower(p)), http.StatusFound)
	case r.Method == http.MethodPut && suffix == "/content":
		if !f.precondition(w, p, conflict, r.Header.Get("If-Match")) {
			return
		}
		if !f.mkParents(p) {
			f.fail(w, http.StatusConflict, "nameAlreadyExists")
			return
		}
		data, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(f.json(f.store(p, data)))
	case r.Method == http.MethodPost && suffix == "/createUploadSession":
		var in struct {
			Item map[string]string `json:"item"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		behaviour := in.Item["@microsoft.graph.conflictBehavior"]
		if !f.precondition(w, p, behaviour, r.Header.Get("If-Match")) {
			return
		}
		f.seq++
		id := strconv.Itoa(f.seq)
		f.sessions[id] = &session{path: p, conflict: behaviour}
		_ = json.NewEncoder(w).Encode(map[string]string{"uploadUrl": f.files.URL + "/upload/" + id})
	case r.Method == http.MethodDelete && suffix == "":
		if it == nil {
			f.fail(w, http.StatusNotFound, "itemNotFound")
			return
		}
		for k := range f.items {
			if k == strings.ToLower(p) || strings.HasPrefix(k, strings.ToLower(p)+"/") {
				delete(f.items, k)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPatch && suffix == "":
		f.move(w, r, p, it, conflict)
	case r.Method == http.MethodPost && suffix == "/children":
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if it == nil || !it.folder {
			f.fail(w, http.StatusNotFound, "itemNotFound")
			return
		}
		name, _ := in["name"].(string)
		child := strings.TrimPrefix(p+"/"+name, "/")
		if f.items[strings.ToLower(child)] != nil {
			f.fail(w, http.StatusConflict, "nameAlreadyExists")
			return
		}
		f.items[strings.ToLower(child)] = &gItem{path: child, folder: true, mod: time.Now()}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(f.json(f.items[strings.ToLower(child)]))
	case r.Method == http.MethodGet && suffix == "/children":
		f.children(w, r, p, it)
	default:
		http.Error(w, "unexpected "+r.Method+" "+suffix, http.StatusBadRequest)
	}
}

func (f *fake) move(w http.ResponseWriter, r *http.Request, p string, it *gItem, conflict string) {
	var in struct {
		Parent struct {
			Path string `json:"path"`
		} `json:"parentReference"`
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if it == nil {
		f.fail(w, http.StatusNotFound, "itemNotFound")
		return
	}
	parent, ok := strings.CutPrefix(in.Parent.Path, "/drive/root")
	if !ok {
		http.Error(w, "bad parentReference.path "+in.Parent.Path, http.StatusBadRequest)
		return
	}
	parent = strings.TrimPrefix(parent, ":/")
	if pi := f.items[strings.ToLower(parent)]; pi == nil || !pi.folder {
		f.fail(w, http.StatusNotFound, "itemNotFound")
		return
	}
	dst := strings.TrimPrefix(parent+"/"+in.Name, "/")
	if f.items[strings.ToLower(dst)] != nil && strings.ToLower(dst) != strings.ToLower(p) {
		if conflict != "replace" {
			f.fail(w, http.StatusConflict, "nameAlreadyExists")
			return
		}
	}
	delete(f.items, strings.ToLower(p))
	it.path = dst
	f.items[strings.ToLower(dst)] = it
	_ = json.NewEncoder(w).Encode(f.json(it))
}

// childPage is small so the driver must follow nextLink.
const childPage = 2

func (f *fake) children(w http.ResponseWriter, r *http.Request, p string, it *gItem) {
	if it == nil || !it.folder {
		f.fail(w, http.StatusNotFound, "itemNotFound")
		return
	}
	var kids []*gItem
	for k, c := range f.items {
		parent := path.Dir(c.path)
		if parent == "." {
			parent = ""
		}
		if k != "" && strings.EqualFold(parent, p) {
			kids = append(kids, c)
		}
	}
	sort.Slice(kids, func(i, j int) bool { return kids[i].path < kids[j].path })
	start, _ := strconv.Atoi(r.URL.Query().Get("$skiptoken"))
	end := min(start+childPage, len(kids))
	out := map[string]any{}
	var value []map[string]any
	for _, c := range kids[start:end] {
		value = append(value, f.json(c))
	}
	out["value"] = value
	if end < len(kids) {
		q := r.URL.Query()
		q.Set("$skiptoken", strconv.Itoa(end))
		out["@odata.nextLink"] = f.graph.URL + r.URL.Path + "?" + q.Encode()
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fake) serveFiles(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := r.Header.Get("Authorization"); a != "" {
		f.leaked = append(f.leaked, r.Method+" "+r.URL.Path+" "+a)
		http.Error(w, "pre-authenticated URL got a token", http.StatusUnauthorized)
		return
	}
	if f.intercept != nil && f.intercept(w, r) {
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/download/"):
		it := f.items[r.URL.Query().Get("p")]
		if it == nil {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "", it.mod, bytes.NewReader(it.data))
	case strings.HasPrefix(r.URL.Path, "/upload/"):
		id := strings.TrimPrefix(r.URL.Path, "/upload/")
		s := f.sessions[id]
		if s == nil {
			f.fail(w, http.StatusNotFound, "itemNotFound")
			return
		}
		if r.Method == http.MethodDelete {
			delete(f.sessions, id)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		cr := r.Header.Get("Content-Range")
		f.contentRanges = append(f.contentRanges, cr)
		var start, end int
		var total string
		if _, err := fmt.Sscanf(cr, "bytes %d-%d/%s", &start, &end, &total); err != nil || start != len(s.data) {
			f.fail(w, http.StatusRequestedRangeNotSatisfiable, "invalidRange")
			return
		}
		data, _ := io.ReadAll(r.Body)
		if len(data) != end-start+1 {
			f.fail(w, http.StatusBadRequest, "invalidRange")
			return
		}
		s.data = append(s.data, data...)
		if total == "*" || strconv.Itoa(len(s.data)) != total {
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"nextExpectedRanges": []string{fmt.Sprintf("%d-", len(s.data))}})
			return
		}
		delete(f.sessions, id)
		if s.conflict == "fail" && f.items[strings.ToLower(s.path)] != nil {
			f.fail(w, http.StatusConflict, "nameAlreadyExists")
			return
		}
		f.mkParents(s.path)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(f.json(f.store(s.path, s.data)))
	default:
		http.NotFound(w, r)
	}
}
