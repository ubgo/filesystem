package fakes

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
	"time"
)

// dbFile is one stored file. Dropbox paths are case-insensitive, so files
// are keyed by their lower-cased path and keep the first casing.
type dbFile struct {
	display string
	data    []byte
	rev     string
	mod     time.Time
}

// Dropbox API v2 endpoints the fake serves, relative to /2/. Some are
// named twice in the dispatch (finish also appends; move also copies), so
// they are constants rather than repeated literals.
const (
	dbGetMetadata   = "files/get_metadata"
	dbDownload      = "files/download"
	dbUpload        = "files/upload"
	dbSessionStart  = "files/upload_session/start"
	dbSessionAppend = "files/upload_session/append_v2"
	dbSessionFinish = "files/upload_session/finish"
	dbDelete        = "files/delete_v2"
	dbCopy          = "files/copy_v2"
	dbMove          = "files/move_v2"
	dbListFolder    = "files/list_folder"
)

// Dropbox WriteMode tags the fake checks; "overwrite", the third, needs
// no check.
const (
	dbModeAdd    = "add"
	dbModeUpdate = "update"
)

// Values of the ".tag" field of a metadata object.
const (
	dbTagFile   = "file"
	dbTagFolder = "folder"
)

// Dropbox error summaries the fake answers with; the driver maps them by
// prefix to filesystem error codes.
const (
	dbErrNotFound       = "path/not_found/"
	dbErrConflict       = "path/conflict/file/"
	dbErrLookupNotFound = "path_lookup/not_found/"
)

// dbArg is the union of every argument object the served endpoints take,
// whether it arrives in the Dropbox-API-Arg header or as the JSON body.
// Each endpoint reads only its own fields:
//   - get_metadata, download, delete_v2, list_folder: Path
//   - upload: Path and Mode (the argument is itself a CommitInfo)
//   - copy_v2, move_v2: FromPath and ToPath
//   - upload_session/append_v2: Cursor
//   - upload_session/finish: Cursor and Commit
type dbArg struct {
	Path     string   `json:"path"`
	Mode     dbMode   `json:"mode"`
	FromPath string   `json:"from_path"`
	ToPath   string   `json:"to_path"`
	Cursor   dbCursor `json:"cursor"`
	Commit   dbCommit `json:"commit"`
}

// dbMode is the WriteMode union: Tag is add, overwrite or update, and
// Update carries the rev an update must match.
type dbMode struct {
	Tag    string `json:".tag"`
	Update string `json:"update"`
}

// dbCommit is a CommitInfo: where a body is stored and how.
type dbCommit struct {
	Path string `json:"path"`
	Mode dbMode `json:"mode"`
}

// dbCursor names an upload session and the offset the next chunk starts at.
type dbCursor struct {
	SessionID string `json:"session_id"`
	Offset    int    `json:"offset"`
}

// dbMetadata is a file or folder metadata object; a folder carries only
// Tag and PathDisplay.
type dbMetadata struct {
	Tag            string `json:".tag"`
	PathDisplay    string `json:"path_display"`
	Rev            string `json:"rev,omitempty"`
	Size           int    `json:"size,omitempty"`
	ServerModified string `json:"server_modified,omitempty"`
}

// dbError is the body of every Dropbox error answer.
type dbError struct {
	Summary string `json:"error_summary"`
}

// dbSession is the answer to upload_session/start.
type dbSession struct {
	SessionID string `json:"session_id"`
}

// dbList is the answer to list_folder: always one page.
type dbList struct {
	Entries []dbMetadata `json:"entries"`
	Cursor  string       `json:"cursor"`
	HasMore bool         `json:"has_more"`
}

// dbEmpty is the empty object delete, copy and move answer with.
type dbEmpty struct{}

// Dropbox is an in-process Dropbox API v2: metadata, download, upload,
// upload sessions, delete, copy, move and list_folder, with the
// documented error shape (409 and an error_summary) and write modes.
type Dropbox struct {
	*httptest.Server
	mu       sync.Mutex
	files    map[string]*dbFile
	sessions map[string][]byte
	seq      int
	chunks   int
}

// NewDropbox starts the fake; Close stops it.
func NewDropbox() *Dropbox {
	f := &Dropbox{files: map[string]*dbFile{}, sessions: map[string][]byte{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// APIURL is the fake's Config.APIURL and Config.ContentURL (one host
// serves both).
func (f *Dropbox) APIURL() string { return f.URL + "/2" }

// Chunks counts the upload-session requests (start, append, finish).
func (f *Dropbox) Chunks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chunks
}

func (f *Dropbox) fail(w http.ResponseWriter, status int, summary string) {
	writeJSON(w, status, dbError{Summary: summary})
}

func (f *Dropbox) meta(file *dbFile) dbMetadata {
	return dbMetadata{Tag: dbTagFile, PathDisplay: file.display, Rev: file.rev, Size: len(file.data), ServerModified: file.mod.UTC().Format(time.RFC3339)}
}

// isFolder reports whether any file lives under p.
func (f *Dropbox) isFolder(p string) bool {
	for k := range f.files {
		if strings.HasPrefix(k, strings.ToLower(p)+"/") {
			return true
		}
	}
	return false
}

func (f *Dropbox) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !authorised(r) {
		f.fail(w, http.StatusUnauthorized, "invalid_access_token/")
		return
	}
	endpoint := strings.TrimPrefix(r.URL.Path, "/2/")
	var arg dbArg
	var body []byte
	if raw := r.Header.Get("Dropbox-API-Arg"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &arg)
		body, _ = io.ReadAll(r.Body)
	} else {
		_ = json.NewDecoder(r.Body).Decode(&arg)
	}
	switch endpoint {
	case dbGetMetadata:
		p := arg.Path
		switch file := f.files[strings.ToLower(p)]; {
		case file != nil:
			writeJSON(w, http.StatusOK, f.meta(file))
		case f.isFolder(p):
			writeJSON(w, http.StatusOK, dbMetadata{Tag: dbTagFolder, PathDisplay: p})
		default:
			f.fail(w, http.StatusConflict, dbErrNotFound)
		}
	case dbDownload:
		file := f.files[strings.ToLower(arg.Path)]
		if file == nil {
			f.fail(w, http.StatusConflict, dbErrNotFound)
			return
		}
		http.ServeContent(w, r, "", file.mod, strings.NewReader(string(file.data)))
	case dbUpload:
		f.commit(w, dbCommit{Path: arg.Path, Mode: arg.Mode}, body)
	case dbSessionStart:
		f.chunks++
		f.seq++
		id := fmt.Sprintf("session-%d", f.seq)
		f.sessions[id] = body
		writeJSON(w, http.StatusOK, dbSession{SessionID: id})
	case dbSessionAppend, dbSessionFinish:
		f.chunks++
		f.appendChunk(w, endpoint, arg, body)
	case dbDelete:
		p, n := strings.ToLower(arg.Path), 0
		for k := range f.files {
			if k == p || strings.HasPrefix(k, p+"/") {
				delete(f.files, k)
				n++
			}
		}
		if n == 0 {
			f.fail(w, http.StatusConflict, dbErrLookupNotFound)
			return
		}
		writeJSON(w, http.StatusOK, dbEmpty{})
	case dbCopy, dbMove:
		f.relocate(w, endpoint, arg.FromPath, arg.ToPath)
	case dbListFolder:
		f.list(w, arg.Path)
	default:
		http.NotFound(w, r)
	}
}

func (f *Dropbox) appendChunk(w http.ResponseWriter, endpoint string, arg dbArg, body []byte) {
	id := arg.Cursor.SessionID
	buf, ok := f.sessions[id]
	if !ok || arg.Cursor.Offset != len(buf) {
		f.fail(w, http.StatusConflict, "lookup_failed/incorrect_offset/")
		return
	}
	f.sessions[id] = append(buf, body...)
	if endpoint == dbSessionFinish {
		data := f.sessions[id]
		delete(f.sessions, id)
		f.commit(w, arg.Commit, data)
		return
	}
	_, _ = w.Write([]byte("null"))
}

func (f *Dropbox) relocate(w http.ResponseWriter, endpoint, from, to string) {
	file := f.files[strings.ToLower(from)]
	if file == nil {
		f.fail(w, http.StatusConflict, "from_lookup/not_found/")
		return
	}
	if f.files[strings.ToLower(to)] != nil && !strings.EqualFold(to, from) {
		f.fail(w, http.StatusConflict, "to/conflict/file/")
		return
	}
	f.seq++
	cp := *file
	cp.display, cp.rev = to, strconv.Itoa(f.seq)
	if endpoint == dbMove {
		delete(f.files, strings.ToLower(from))
	}
	f.files[strings.ToLower(to)] = &cp
	writeJSON(w, http.StatusOK, dbEmpty{})
}

// list answers a recursive list_folder in one page.
func (f *Dropbox) list(w http.ResponseWriter, p string) {
	if !f.isFolder(p) {
		f.fail(w, http.StatusConflict, dbErrNotFound)
		return
	}
	var keys []string
	for k := range f.files {
		if strings.HasPrefix(k, strings.ToLower(p)+"/") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	entries := []dbMetadata{}
	for _, k := range keys {
		entries = append(entries, f.meta(f.files[k]))
	}
	writeJSON(w, http.StatusOK, dbList{Entries: entries, Cursor: "end"})
}

// commit stores data under commit's path, honouring its write mode: add
// fails when the file exists, update when its rev has changed.
func (f *Dropbox) commit(w http.ResponseWriter, commit dbCommit, data []byte) {
	p := commit.Path
	existing := f.files[strings.ToLower(p)]
	switch commit.Mode.Tag {
	case dbModeAdd:
		if existing != nil {
			f.fail(w, http.StatusConflict, dbErrConflict)
			return
		}
	case dbModeUpdate:
		if existing == nil || existing.rev != commit.Mode.Update {
			f.fail(w, http.StatusConflict, dbErrConflict)
			return
		}
	}
	f.seq++
	display := p
	if existing != nil {
		display = existing.display // Dropbox keeps the first casing
	}
	file := &dbFile{display: display, data: data, rev: fmt.Sprintf("rev%06d", f.seq), mod: time.Now()}
	f.files[strings.ToLower(p)] = file
	writeJSON(w, http.StatusOK, f.meta(file))
}
