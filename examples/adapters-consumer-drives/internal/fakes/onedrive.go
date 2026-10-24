package fakes

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
	"time"
)

// oneDrivePath is the Graph path of the one drive the fake serves, the
// signed-in user's (Config.Drive "/me/drive", the default).
const oneDrivePath = "/v1.0/me/drive/root"

// Graph names the fake checks or answers with, each used more than once.
const (
	// odConflictParam carries the conflictBehavior of a PUT (as a query
	// parameter) and of an upload session (as an item property).
	odConflictParam = "@microsoft.graph.conflictBehavior"
	// odConflictFail is the conflictBehavior that refuses an existing
	// item; "replace", the other the driver sends, needs no check.
	odConflictFail = "fail"
	// The item path suffixes the fake routes on.
	odSuffixContent  = "/content"
	odSuffixChildren = "/children"
	// The Graph error codes the driver maps to filesystem codes.
	odErrNotFound   = "itemNotFound"
	odErrNameExists = "nameAlreadyExists"
)

// odDriveItem is a driveItem as the fake answers it: exactly one of
// Folder and File is set.
type odDriveItem struct {
	Name                 string       `json:"name"`
	Size                 int          `json:"size"`
	LastModifiedDateTime string       `json:"lastModifiedDateTime"`
	ETag                 string       `json:"eTag"`
	Folder               *odFolder    `json:"folder,omitempty"`
	File                 *odFileFacet `json:"file,omitempty"`
}

// odFolder is the folder facet of a driveItem.
type odFolder struct {
	ChildCount int `json:"childCount"`
}

// odFileFacet is the file facet of a driveItem.
type odFileFacet struct {
	MimeType string `json:"mimeType"`
}

// odErrorBody is Graph's error envelope.
type odErrorBody struct {
	Error odError `json:"error"`
}

// odError is the error object inside odErrorBody; the driver reads Code.
type odError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// odChildren is the answer to a children request: always one page.
type odChildren struct {
	Value []odDriveItem `json:"value"`
}

// odUploadSession is the answer to createUploadSession.
type odUploadSession struct {
	UploadURL string `json:"uploadUrl"`
}

// odNextRanges is the answer to a non-final upload-session fragment.
type odNextRanges struct {
	NextExpectedRanges []string `json:"nextExpectedRanges"`
}

// odItem is one driveItem; paths are case-insensitive, so items are keyed
// by their lower-cased path and keep the cased one.
type odItem struct {
	path   string
	folder bool
	data   []byte
	etag   int
	mod    time.Time
}

type odSession struct {
	path, conflict string
	data           []byte
}

// OneDrive is an in-process Microsoft Graph drive: item metadata, a 302
// from /content to a pre-authenticated download host, PUT content, upload
// sessions on that other host, conflictBehavior, If-Match, folder
// creation, move (PATCH), delete and children. The download and upload
// host refuses any request carrying a token, as Graph requires.
type OneDrive struct {
	graph, files *httptest.Server
	mu           sync.Mutex
	items        map[string]*odItem
	sessions     map[string]*odSession
	seq          int
	chunks       int
}

// NewOneDrive starts the fake's two hosts; Close stops them.
func NewOneDrive() *OneDrive {
	f := &OneDrive{items: map[string]*odItem{"": {folder: true}}, sessions: map[string]*odSession{}}
	f.graph = httptest.NewServer(http.HandlerFunc(f.serveGraph))
	f.files = httptest.NewServer(http.HandlerFunc(f.serveFiles))
	return f
}

// BaseURL is the fake's Config.BaseURL.
func (f *OneDrive) BaseURL() string { return f.graph.URL + "/v1.0" }

// Close stops both hosts.
func (f *OneDrive) Close() {
	f.graph.Close()
	f.files.Close()
}

// Chunks counts the upload-session fragments received so far.
func (f *OneDrive) Chunks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chunks
}

func (f *OneDrive) fail(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, odErrorBody{Error: odError{Code: code, Message: code}})
}

// eTag is the item's eTag, quoted as Graph sends it.
func (f *OneDrive) eTag(it *odItem) string { return fmt.Sprintf(`"{0000-ETAG},%d"`, it.etag) }

func (f *OneDrive) json(it *odItem) odDriveItem {
	m := odDriveItem{Name: path.Base(it.path), Size: len(it.data), LastModifiedDateTime: it.mod.UTC().Format(time.RFC3339Nano), ETag: f.eTag(it)}
	if it.folder {
		m.Folder = &odFolder{}
	} else {
		m.File = &odFileFacet{MimeType: "application/octet-stream"}
	}
	return m
}

// mkParents creates the folders above p, as Graph does on upload.
func (f *OneDrive) mkParents(p string) {
	dir := path.Dir(p)
	if dir == "." || f.items[strings.ToLower(dir)] != nil {
		return
	}
	f.mkParents(dir)
	f.items[strings.ToLower(dir)] = &odItem{path: dir, folder: true, mod: time.Now()}
}

func (f *OneDrive) store(p string, data []byte) *odItem {
	f.seq++
	f.mkParents(p)
	cased := p
	if old := f.items[strings.ToLower(p)]; old != nil {
		cased = old.path
	}
	it := &odItem{path: cased, data: data, etag: f.seq, mod: time.Now()}
	f.items[strings.ToLower(p)] = it
	return it
}

// precondition checks conflictBehavior and If-Match, writing the error
// and reporting false when the write must not happen.
func (f *OneDrive) precondition(w http.ResponseWriter, p, conflict, ifMatch string) bool {
	existing := f.items[strings.ToLower(p)]
	switch {
	case ifMatch != "" && existing == nil:
		f.fail(w, http.StatusNotFound, odErrNotFound)
	case ifMatch != "" && strings.Trim(ifMatch, `"`) != strings.Trim(f.eTag(existing), `"`):
		f.fail(w, http.StatusPreconditionFailed, "resourceModified")
	case conflict == odConflictFail && existing != nil:
		f.fail(w, http.StatusConflict, odErrNameExists)
	default:
		return true
	}
	return false
}

func (f *OneDrive) serveGraph(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !authorised(r) {
		f.fail(w, http.StatusUnauthorized, "InvalidAuthenticationToken")
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, oneDrivePath)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var p, suffix string
	if rest, ok = strings.CutPrefix(rest, ":/"); ok {
		i := strings.LastIndex(rest, ":")
		if i < 0 {
			http.Error(w, "missing closing colon", http.StatusBadRequest)
			return
		}
		p, suffix = rest[:i], rest[i+1:]
	} else {
		suffix = rest
	}
	it := f.items[strings.ToLower(p)]
	switch {
	case r.Method == http.MethodGet && suffix == "":
		if it == nil {
			f.fail(w, http.StatusNotFound, odErrNotFound)
			return
		}
		writeJSON(w, http.StatusOK, f.json(it))
	case r.Method == http.MethodGet && suffix == odSuffixContent:
		if it == nil || it.folder {
			f.fail(w, http.StatusNotFound, odErrNotFound)
			return
		}
		http.Redirect(w, r, f.files.URL+"/download?p="+url.QueryEscape(strings.ToLower(p)), http.StatusFound)
	case r.Method == http.MethodPut && suffix == odSuffixContent:
		if !f.precondition(w, p, r.URL.Query().Get(odConflictParam), r.Header.Get("If-Match")) {
			return
		}
		data, _ := io.ReadAll(r.Body)
		writeJSON(w, http.StatusCreated, f.json(f.store(p, data)))
	case r.Method == http.MethodPost && suffix == "/createUploadSession":
		f.createSession(w, r, p)
	case r.Method == http.MethodDelete && suffix == "":
		if it == nil {
			f.fail(w, http.StatusNotFound, odErrNotFound)
			return
		}
		for k := range f.items {
			if k == strings.ToLower(p) || strings.HasPrefix(k, strings.ToLower(p)+"/") {
				delete(f.items, k)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPatch && suffix == "":
		f.move(w, r, p, it)
	case r.Method == http.MethodPost && suffix == odSuffixChildren:
		f.mkdir(w, r, p, it)
	case r.Method == http.MethodGet && suffix == odSuffixChildren:
		f.children(w, p, it)
	default:
		http.Error(w, "unexpected "+r.Method+" "+suffix, http.StatusBadRequest)
	}
}

func (f *OneDrive) createSession(w http.ResponseWriter, r *http.Request, p string) {
	var in struct {
		Item struct {
			ConflictBehavior string `json:"@microsoft.graph.conflictBehavior"`
		} `json:"item"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	behaviour := in.Item.ConflictBehavior
	if !f.precondition(w, p, behaviour, r.Header.Get("If-Match")) {
		return
	}
	f.seq++
	id := strconv.Itoa(f.seq)
	f.sessions[id] = &odSession{path: p, conflict: behaviour}
	writeJSON(w, http.StatusOK, odUploadSession{UploadURL: f.files.URL + "/upload/" + id})
}

func (f *OneDrive) mkdir(w http.ResponseWriter, r *http.Request, p string, parent *odItem) {
	var in struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if parent == nil || !parent.folder {
		f.fail(w, http.StatusNotFound, odErrNotFound)
		return
	}
	child := strings.TrimPrefix(p+"/"+in.Name, "/")
	if f.items[strings.ToLower(child)] != nil {
		f.fail(w, http.StatusConflict, odErrNameExists)
		return
	}
	f.items[strings.ToLower(child)] = &odItem{path: child, folder: true, mod: time.Now()}
	writeJSON(w, http.StatusCreated, f.json(f.items[strings.ToLower(child)]))
}

func (f *OneDrive) move(w http.ResponseWriter, r *http.Request, p string, it *odItem) {
	var in struct {
		Parent struct {
			Path string `json:"path"`
		} `json:"parentReference"`
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	parent, ok := strings.CutPrefix(in.Parent.Path, "/drive/root")
	if it == nil || !ok {
		f.fail(w, http.StatusNotFound, odErrNotFound)
		return
	}
	dst := strings.TrimPrefix(strings.TrimPrefix(parent, ":/")+"/"+in.Name, "/")
	delete(f.items, strings.ToLower(p))
	it.path = dst
	f.items[strings.ToLower(dst)] = it
	writeJSON(w, http.StatusOK, f.json(it))
}

// children answers one page holding every child of p.
func (f *OneDrive) children(w http.ResponseWriter, p string, it *odItem) {
	if it == nil || !it.folder {
		f.fail(w, http.StatusNotFound, odErrNotFound)
		return
	}
	var kids []*odItem
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
	value := []odDriveItem{}
	for _, c := range kids {
		value = append(value, f.json(c))
	}
	writeJSON(w, http.StatusOK, odChildren{Value: value})
}

// serveFiles is the pre-authenticated host: downloads and session
// fragments, which must arrive without a token.
func (f *OneDrive) serveFiles(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "" {
		http.Error(w, "a pre-authenticated URL got a token", http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/download" {
		it := f.items[r.URL.Query().Get("p")]
		if it == nil {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "", it.mod, bytes.NewReader(it.data))
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/upload/")
	s := f.sessions[id]
	if s == nil {
		f.fail(w, http.StatusNotFound, odErrNotFound)
		return
	}
	if r.Method == http.MethodDelete {
		delete(f.sessions, id)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	f.chunks++
	var start, end int
	var total string
	if _, err := fmt.Sscanf(r.Header.Get("Content-Range"), "bytes %d-%d/%s", &start, &end, &total); err != nil || start != len(s.data) {
		f.fail(w, http.StatusRequestedRangeNotSatisfiable, "invalidRange")
		return
	}
	data, _ := io.ReadAll(r.Body)
	s.data = append(s.data, data...)
	if total == "*" || strconv.Itoa(len(s.data)) != total {
		writeJSON(w, http.StatusAccepted, odNextRanges{NextExpectedRanges: []string{fmt.Sprintf("%d-", len(s.data))}})
		return
	}
	delete(f.sessions, id)
	if s.conflict == odConflictFail && f.items[strings.ToLower(s.path)] != nil {
		f.fail(w, http.StatusConflict, odErrNameExists)
		return
	}
	writeJSON(w, http.StatusCreated, f.json(f.store(s.path, s.data)))
}
