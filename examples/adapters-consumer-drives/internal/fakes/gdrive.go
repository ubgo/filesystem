package fakes

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
	"time"
)

// GDriveFolderID is the ID of the application's folder in the Drive fake,
// the Config.FolderID a drive.file-scoped application would pass.
const GDriveFolderID = "appfolder"

// gdriveFolderMIME is the MIME type Drive gives folders.
const gdriveFolderMIME = "application/vnd.google-apps.folder"

// statusResumeIncomplete is Drive's answer to a non-final upload chunk.
const statusResumeIncomplete = 308

// Paths the fake serves: metadata calls under gFilesPath, resumable
// uploads under gUploadPath.
const (
	gFilesPath  = "/drive/v3/files"
	gUploadPath = "/upload/drive/v3/files"
)

// gMeta is the file metadata a request carries: the body of a create, a
// copy, an update (only Name is read) and the start of a resumable upload.
type gMeta struct {
	Name     string   `json:"name"`
	MimeType string   `json:"mimeType"`
	Parents  []string `json:"parents"`
}

// gFile is a Drive file resource as the fake answers it; folders carry no
// Size or MD5Checksum.
type gFile struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MimeType     string `json:"mimeType"`
	ModifiedTime string `json:"modifiedTime"`
	Size         string `json:"size,omitempty"`
	MD5Checksum  string `json:"md5Checksum,omitempty"`
}

// gFileList is the answer to files.list: always one page.
type gFileList struct {
	Files []gFile `json:"files"`
}

// gErrorBody is Drive's error envelope; the driver reads Errors[0].Reason.
type gErrorBody struct {
	Error gError `json:"error"`
}

// gError is the error object inside gErrorBody.
type gError struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Errors  []gErrorCause `json:"errors"`
}

// gErrorCause is one entry of gError.Errors.
type gErrorCause struct {
	Reason string `json:"reason"`
}

type gItem struct {
	id, name, parent, mime string
	data                   []byte
	created                int
	mod                    time.Time
}

type gUpload struct {
	target string // file ID for a new revision, "" for a new file
	meta   gMeta
	data   []byte
}

// GDrive is an in-process Drive API v3: files.list with the query forms
// the driver writes, alt=media, folder creation, copy, update with
// addParents/removeParents, delete and resumable uploads.
type GDrive struct {
	*httptest.Server
	mu      sync.Mutex
	items   map[string]*gItem
	uploads map[string]*gUpload
	seq     int
	chunks  int
}

// NewGDrive starts the fake; Close stops it.
func NewGDrive() *GDrive {
	f := &GDrive{items: map[string]*gItem{}, uploads: map[string]*gUpload{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// APIURL is the fake's Config.APIURL: metadata calls go here.
func (f *GDrive) APIURL() string { return f.URL + "/drive/v3" }

// UploadURL is the fake's Config.UploadURL: Drive takes file bodies on a
// separate upload path, so the driver is configured with both.
func (f *GDrive) UploadURL() string { return f.URL + "/upload/drive/v3" }

// Chunks counts the upload chunks received so far.
func (f *GDrive) Chunks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chunks
}

func (f *GDrive) create(name, parent, mime string, data []byte) *gItem {
	f.seq++
	if mime == "" {
		mime = "application/octet-stream"
	}
	it := &gItem{id: fmt.Sprintf("id%04d", f.seq), name: name, parent: parent, mime: mime, data: data, created: f.seq, mod: time.Now()}
	f.items[it.id] = it
	return it
}

func (f *GDrive) fail(w http.ResponseWriter, status int, reason string) {
	writeJSON(w, status, gErrorBody{Error: gError{Code: status, Message: reason, Errors: []gErrorCause{{Reason: reason}}}})
}

func (f *GDrive) json(it *gItem) gFile {
	m := gFile{ID: it.id, Name: it.name, MimeType: it.mime, ModifiedTime: it.mod.UTC().Format(time.RFC3339Nano)}
	if it.mime != gdriveFolderMIME {
		sum := md5.Sum(it.data)
		m.Size, m.MD5Checksum = strconv.Itoa(len(it.data)), hex.EncodeToString(sum[:])
	}
	return m
}

var gListQuery = regexp.MustCompile(`^'((?:[^'\\]|\\.)*)' in parents and trashed = false(?: and name = '((?:[^'\\]|\\.)*)')?$`)

func gUnquote(s string) string { return strings.NewReplacer(`\\`, `\`, `\'`, `'`).Replace(s) }

func (f *GDrive) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !authorised(r) {
		f.fail(w, http.StatusUnauthorized, "authError")
		return
	}
	switch p := r.URL.Path; {
	case p == gFilesPath && r.Method == http.MethodGet:
		f.list(w, r)
	case p == gFilesPath && r.Method == http.MethodPost:
		var in gMeta
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Parents) != 1 {
			f.fail(w, http.StatusBadRequest, "badRequest")
			return
		}
		writeJSON(w, http.StatusOK, f.json(f.create(in.Name, in.Parents[0], in.MimeType, nil)))
	case strings.HasPrefix(p, gFilesPath+"/"):
		f.file(w, r, strings.TrimPrefix(p, gFilesPath+"/"))
	case strings.HasPrefix(p, gUploadPath):
		f.upload(w, r, strings.TrimPrefix(strings.TrimPrefix(p, gUploadPath), "/"))
	default:
		http.NotFound(w, r)
	}
}

func (f *GDrive) list(w http.ResponseWriter, r *http.Request) {
	m := gListQuery.FindStringSubmatch(r.URL.Query().Get("q"))
	if m == nil {
		f.fail(w, http.StatusBadRequest, "invalidQuery")
		return
	}
	parent, byName, name := gUnquote(m[1]), strings.Contains(r.URL.Query().Get("q"), " and name = "), gUnquote(m[2])
	var kids []*gItem
	for _, it := range f.items {
		if it.parent == parent && (!byName || it.name == name) {
			kids = append(kids, it)
		}
	}
	sort.Slice(kids, func(i, j int) bool { return kids[i].created < kids[j].created })
	files := []gFile{}
	for _, it := range kids {
		files = append(files, f.json(it))
	}
	writeJSON(w, http.StatusOK, gFileList{Files: files})
}

func (f *GDrive) file(w http.ResponseWriter, r *http.Request, rest string) {
	id, op, _ := strings.Cut(rest, "/")
	it := f.items[id]
	if it == nil {
		f.fail(w, http.StatusNotFound, "notFound")
		return
	}
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && q.Get("alt") == "media":
		http.ServeContent(w, r, "", it.mod, bytes.NewReader(it.data))
	case r.Method == http.MethodPost && op == "copy":
		var in gMeta
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Parents) != 1 {
			f.fail(w, http.StatusBadRequest, "badRequest")
			return
		}
		writeJSON(w, http.StatusOK, f.json(f.create(in.Name, in.Parents[0], it.mime, it.data)))
	case r.Method == http.MethodPatch:
		var in gMeta
		_ = json.NewDecoder(r.Body).Decode(&in)
		if add := q.Get("addParents"); add != "" {
			it.parent = add
		}
		if in.Name != "" {
			it.name = in.Name
		}
		writeJSON(w, http.StatusOK, f.json(it))
	case r.Method == http.MethodDelete:
		f.removeTree(id)
		w.WriteHeader(http.StatusNoContent)
	default:
		f.fail(w, http.StatusBadRequest, "unexpected")
	}
}

func (f *GDrive) removeTree(id string) {
	delete(f.items, id)
	for k, it := range f.items {
		if it.parent == id {
			f.removeTree(k)
		}
	}
}

// upload starts a resumable session, or takes a chunk of one.
func (f *GDrive) upload(w http.ResponseWriter, r *http.Request, id string) {
	if sid := r.URL.Query().Get("upload_id"); sid != "" {
		f.chunk(w, r, sid)
		return
	}
	var meta gMeta
	_ = json.NewDecoder(r.Body).Decode(&meta)
	f.seq++
	sid := strconv.Itoa(f.seq)
	f.uploads[sid] = &gUpload{target: id, meta: meta}
	w.Header().Set("Location", f.URL+gUploadPath+"?uploadType=resumable&upload_id="+sid)
	w.WriteHeader(http.StatusOK)
}

func (f *GDrive) chunk(w http.ResponseWriter, r *http.Request, sid string) {
	up := f.uploads[sid]
	if up == nil {
		f.fail(w, http.StatusNotFound, "notFound")
		return
	}
	if r.Method == http.MethodDelete {
		delete(f.uploads, sid)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	f.chunks++
	cr := r.Header.Get("Content-Range")
	data, _ := io.ReadAll(r.Body)
	total, ok := strings.CutPrefix(cr, "bytes */")
	if !ok {
		var start, end int
		if _, err := fmt.Sscanf(cr, "bytes %d-%d/%s", &start, &end, &total); err != nil || start != len(up.data) {
			f.fail(w, http.StatusBadRequest, "badContentRange")
			return
		}
	}
	up.data = append(up.data, data...)
	if total == "*" || total != strconv.Itoa(len(up.data)) {
		w.WriteHeader(statusResumeIncomplete)
		return
	}
	delete(f.uploads, sid)
	mime := up.meta.MimeType
	if up.target != "" {
		it := f.items[up.target]
		it.data, it.mod = up.data, time.Now()
		if mime != "" {
			it.mime = mime
		}
		writeJSON(w, http.StatusOK, f.json(it))
		return
	}
	if len(up.meta.Parents) != 1 {
		f.fail(w, http.StatusBadRequest, "badParents")
		return
	}
	writeJSON(w, http.StatusOK, f.json(f.create(up.meta.Name, up.meta.Parents[0], mime, up.data)))
}
