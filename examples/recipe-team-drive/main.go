// Command recipe-team-drive is the core of a team drive: each workspace
// has a storage quota from its plan, files live in folders, replacing a
// file keeps its previous contents as versions that can be restored, files
// can be shared with people outside the workspace through expiring links,
// and every change lands in an activity feed with who made it.
//
//	go run ./recipe-team-drive       (listens on localhost:8110)
//
// The signed-in workspace and user are the X-Workspace and X-User
// headers (demo only). Routes:
//
//	POST /folders                    {"parent": "", "name": "Contracts"}
//	POST /files?folder={id}          multipart form, field "file"
//	GET  /folders/{id}/files         files in a folder ("root" for the top)
//	PUT  /files/{id}                 new content; the old one becomes a version
//	GET  /files/{id}/versions        previous contents, newest first
//	POST /versions/{id}/restore      make a version current again
//	POST /files/{id}/share           {"ttl_seconds": 3600, "max_downloads": 3}
//	GET  /activity                   who changed what
//	GET  /usage                      bytes and files used, and the limit
//	GET  /s/{token}                  a share link (no sign-in)
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/examples/internal/serve"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/audit"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/filesystem/records/share"
	"github.com/ubgo/filesystem/records/versions"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this app, not of the library.
const (
	// listenAddr is where the demo server listens.
	listenAddr = "localhost:8110"
	// baseURL is the server's public origin; signed links start with it.
	baseURL = "http://" + listenAddr
	// signedPrefix is where fshttp.Signed serves signed links.
	signedPrefix = "/signed"
	// diskName names the drive's disk.
	diskName = "drive"
	// headerWorkspace and headerUser carry the session. Demo only.
	headerWorkspace = "X-Workspace"
	headerUser      = "X-User"
	// rootAlias names the root folder in URLs, whose ID is empty.
	rootAlias = "root"
	// maxFileBytes caps one upload.
	maxFileBytes = 10 << 20
	// keptVersions is how many previous contents each file keeps.
	keptVersions = 5
	// maxShareTTL is the longest a share link may live.
	maxShareTTL = 7 * 24 * time.Hour
	// maxJSONBody caps a JSON request body.
	maxJSONBody = 64 << 10
	// readHeaderTimeout bounds a slow client's headers.
	readHeaderTimeout = 10 * time.Second
)

// plans are the storage limits per plan; planOf picks a workspace's plan.
// The free plan is deliberately tiny so the demo can hit it.
// ds:def id=plans-mjbvbvf8
var plans = map[string]quota.Limit{
	"free": {Bytes: 1 << 20, Files: 100},
	"team": {Bytes: 100 << 30, Files: 1_000_000},
}

// teamWorkspaces are the workspaces on the team plan; every other one is
// on the free plan. A real app reads the plan from its billing table.
var teamWorkspaces = map[string]bool{"acme": true}

// planLimit is the quota plugin's LimitFunc: the limit of a workspace's
// plan, asked on every upload, so a plan change applies at once.
// ds:def id=planlimit-cfcrukp2
func planLimit(_ context.Context, workspace string) (quota.Limit, error) {
	if teamWorkspaces[workspace] {
		return plans["team"], nil
	}
	return plans["free"], nil
}

// policy accepts documents and images, never active content.
// ds:def id=policy-vhwdjca2
var policy = upload.Policy{
	MaxSize: maxFileBytes,
	Allowed: []string{"image/*", "application/pdf", "text/plain", "text/csv", "application/zip"},
	Key:     upload.RandomKey("files"),
}

// App holds the wired dependencies.
type App struct {
	Store    *records.Store
	Quota    *quota.Plugin
	Folders  *folders.Plugin
	Versions *versions.Plugin
	Shares   *share.Plugin
	Audit    *audit.Plugin
	Handler  http.Handler
}

// newPlugins builds the plugins this drive uses. Order matters only for
// migrations, which run in installation order.
// ds:def id=newplugins-99f3mrb9
func newPlugins() (*App, error) {
	q, err := quota.New(planLimit)
	if err != nil {
		return nil, err
	}
	v, err := versions.New(versions.WithKeep(keptVersions))
	if err != nil {
		return nil, err
	}
	return &App{Quota: q, Folders: folders.New(), Versions: v, Shares: share.New(), Audit: audit.New()}, nil
}

// openStore opens the disk (signed links through fshttp.Signed), SQLite
// and the Store with every plugin installed.
// ds:def id=app-openstore-uzs4x4gh
func (a *App) openStore(ctx context.Context, dataDir, base string, secret []byte) (*filesystem.Manager, *signedurl.Signer, error) {
	signer, err := signedurl.New(base+signedPrefix, []signedurl.Key{{ID: "k1", Secret: secret}})
	if err != nil {
		return nil, nil, err
	}
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv, filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "drive.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	a.Store, err = records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithPlugins(a.Quota, a.Versions, a.Folders, a.Shares, a.Audit))
	if err != nil {
		return nil, nil, err
	}
	_, err = a.Store.Migrate(ctx)
	return disks, signer, err
}

// New wires the drive over a data directory, its public origin and the
// signing secret.
func New(ctx context.Context, dataDir, base string, secret []byte) (*App, error) {
	a, err := newPlugins()
	if err != nil {
		return nil, err
	}
	disks, signer, err := a.openStore(ctx, dataDir, base, secret)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /folders", a.createFolder)
	mux.HandleFunc("POST /files", a.uploadFile)
	mux.HandleFunc("GET /folders/{id}/files", a.listFolder)
	mux.HandleFunc("PUT /files/{id}", a.replaceFile)
	mux.HandleFunc("GET /files/{id}/versions", a.listVersions)
	mux.HandleFunc("POST /versions/{id}/restore", a.restoreVersion)
	mux.HandleFunc("POST /files/{id}/share", a.shareFile)
	mux.HandleFunc("GET /activity", a.activity)
	mux.HandleFunc("GET /usage", a.usage)
	mux.Handle("/s/", http.StripPrefix("/s/", share.Handler(a.Store, a.Shares)))
	mux.Handle(signedPrefix+"/", http.StripPrefix(signedPrefix, fshttp.Signed(disks, signer)))
	a.Handler = withActor(mux)
	return a, nil
}

// withActor records the signed-in user as the actor of every change the
// request makes: File.UploadedBy and the audit log read it.
// ds:def id=withactor-r6bu5mf8
func withActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user := r.Header.Get(headerUser); user != "" {
			r = r.WithContext(records.WithActor(r.Context(), "user:"+user))
		}
		next.ServeHTTP(w, r)
	})
}

// workspaceOf returns the signed-in workspace: the scope of every call.
func workspaceOf(r *http.Request) (string, error) {
	if ws := r.Header.Get(headerWorkspace); ws != "" {
		return ws, nil
	}
	return "", filesystem.Errorf(filesystem.CodePermission, "not signed in")
}

// folderParam maps the URL's folder name to a folder ID.
func folderParam(v string) string {
	if v == rootAlias {
		return folders.RootID
	}
	return v
}

// handle runs fn in the request's workspace with the decoded JSON body (if
// any) and answers with its result or its error.
// ds:def id=handle-ttrqu2ur
func handle[In, Out any](w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, ws string, in In) (Out, error)) {
	ws, err := workspaceOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var in In
	if r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&in); err != nil {
			writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "invalid JSON body"))
			return
		}
	}
	out, err := fn(r.Context(), ws, in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// folderRequest is the body of createFolder.
type folderRequest struct {
	Parent string `json:"parent"`
	Name   string `json:"name"`
}

// folderResponse is the JSON of a folder.
type folderResponse struct {
	ID     string `json:"id"`
	Parent string `json:"parent"`
	Name   string `json:"name"`
}

func (a *App) createFolder(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, ws string, in folderRequest) (folderResponse, error) {
		f, err := a.Folders.Create(ctx, a.Store, ws, folderParam(in.Parent), in.Name)
		return folderResponse{ID: f.ID, Parent: f.ParentID, Name: f.Name}, err
	})
}

// store uploads a file into a folder in one transaction: the quota is
// charged when the row is inserted, and a full workspace
// (CodeQuotaExceeded) or an unknown folder rolls back and deletes the
// stored bytes.
// ds:def id=app-store-yfye774s
func (a *App) store(ctx context.Context, ws, folder string, body io.Reader, filename string) (records.File, error) {
	var f records.File
	err := a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var err error
		if f, err = a.Store.Upload(ctx, ws, diskName, body, filename, policy); err != nil {
			return err
		}
		if folder == folders.RootID {
			return nil
		}
		return a.Folders.Put(ctx, a.Store, ws, f.ID, folder)
	})
	return f, err
}

// formFile opens the uploaded file of a request, capped at the policy's
// size plus form overhead.
func formFile(w http.ResponseWriter, r *http.Request) (io.ReadCloser, string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFileBytes+maxJSONBody)
	file, header, err := r.FormFile("file")
	if err != nil {
		return nil, "", filesystem.Errorf(filesystem.CodeInvalidArgument, "send the file in the form field \"file\"")
	}
	return file, header.Filename, nil
}

// fileResponse is the JSON of a stored or replaced file.
type fileResponse struct {
	ID         string `json:"id"`
	Filename   string `json:"filename"`
	Size       int64  `json:"size"`
	UploadedBy string `json:"uploaded_by"`
}

func toResponse(f records.File) fileResponse {
	return fileResponse{ID: f.ID, Filename: f.Filename, Size: f.Size, UploadedBy: f.UploadedBy}
}

func (a *App) uploadFile(w http.ResponseWriter, r *http.Request) {
	ws, err := workspaceOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	file, name, err := formFile(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	defer func() { _ = file.Close() }()
	f, err := a.store(r.Context(), ws, folderParam(r.URL.Query().Get("folder")), file, name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toResponse(f))
}

// replaceFile gives a file new content. The file keeps its ID, folder and
// share links; the versions plugin keeps the previous content, and the
// quota is charged only the size difference.
// ds:def id=app-replacefile-3fex6my6
func (a *App) replaceFile(w http.ResponseWriter, r *http.Request) {
	ws, err := workspaceOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	file, name, err := formFile(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	defer func() { _ = file.Close() }()
	f, err := a.Store.Replace(r.Context(), ws, r.PathValue("id"), file, name, policy)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toResponse(f))
}

type none struct{}

func (a *App) listFolder(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, ws string, _ none) ([]fileResponse, error) {
		page, err := a.Folders.Files(ctx, a.Store, folderParam(r.PathValue("id")), records.FileQuery{Scope: ws, Cursor: r.URL.Query().Get("cursor")})
		out := make([]fileResponse, 0, len(page.Files))
		for _, f := range page.Files {
			out = append(out, toResponse(f))
		}
		return out, err
	})
}

// versionResponse is the JSON of one kept version.
type versionResponse struct {
	ID         string    `json:"id"`
	Number     int64     `json:"number"`
	Filename   string    `json:"filename"`
	Size       int64     `json:"size"`
	ReplacedAt time.Time `json:"replaced_at"`
}

// listVersions checks the file is in the workspace first, so another
// workspace's file ID is a 404 rather than an empty list.
func (a *App) listVersions(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, ws string, _ none) ([]versionResponse, error) {
		if _, err := a.Store.File(ctx, ws, r.PathValue("id")); err != nil {
			return nil, err
		}
		list, err := a.Versions.Versions(ctx, a.Store, ws, r.PathValue("id"))
		out := make([]versionResponse, 0, len(list))
		for _, v := range list {
			out = append(out, versionResponse{ID: v.ID, Number: v.Number, Filename: v.Filename, Size: v.Size, ReplacedAt: v.ReplacedAt})
		}
		return out, err
	})
}

// restoreVersion makes a version current. The content it replaces becomes
// the newest version, so a restore can itself be undone.
// ds:def id=app-restoreversion-czm9ymqs
func (a *App) restoreVersion(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, ws string, _ none) (fileResponse, error) {
		f, err := a.Versions.Restore(ctx, a.Store, ws, r.PathValue("id"))
		return toResponse(f), err
	})
}

// shareRequest and shareResponse are the JSON of shareFile.
type shareRequest struct {
	TTLSeconds   int64 `json:"ttl_seconds"`
	MaxDownloads int64 `json:"max_downloads"`
}

type shareResponse struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// shareFile creates an expiring, download-limited link. The token is in the
// answer once; only its hash is stored.
// ds:def id=app-sharefile-fgqxyp5e
func (a *App) shareFile(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, ws string, in shareRequest) (shareResponse, error) {
		ttl := time.Duration(in.TTLSeconds) * time.Second
		if ttl <= 0 || ttl > maxShareTTL {
			return shareResponse{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "ttl_seconds must be in (0, %d]", int64(maxShareTTL/time.Second))
		}
		sh, err := a.Shares.Create(ctx, a.Store, ws, r.PathValue("id"), share.Options{TTL: ttl, MaxDownloads: in.MaxDownloads})
		return shareResponse{URL: baseURL + "/s/" + sh.Token, ExpiresAt: sh.ExpiresAt}, err
	})
}

// activityEntry is one line of the feed.
// Plugin events (folders, share links) name their folder or link in
// Subject and carry their particulars in Detail; a download through a
// share link is anonymous, so its Actor is empty.
type activityEntry struct {
	At      time.Time         `json:"at"`
	Actor   string            `json:"actor"`
	Kind    records.EventKind `json:"kind"`
	FileID  string            `json:"file_id,omitempty"`
	Subject string            `json:"subject,omitempty"`
	Detail  map[string]string `json:"detail,omitempty"`
}

// ds:def id=app-activity-sjmebe8n
func (a *App) activity(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, ws string, _ none) ([]activityEntry, error) {
		page, err := a.Audit.Entries(ctx, a.Store, audit.Query{Scope: ws, Cursor: r.URL.Query().Get("cursor")})
		out := make([]activityEntry, 0, len(page.Entries))
		for _, e := range page.Entries {
			out = append(out, activityEntry{At: e.At, Actor: e.Actor, Kind: e.Kind, FileID: e.FileID, Subject: e.Subject, Detail: e.Detail})
		}
		return out, err
	})
}

// usageResponse is what a billing page shows.
type usageResponse struct {
	UsedBytes  int64 `json:"used_bytes"`
	UsedFiles  int64 `json:"used_files"`
	LimitBytes int64 `json:"limit_bytes"`
	LimitFiles int64 `json:"limit_files"`
}

// ds:def id=app-usage-amdzfg2p
func (a *App) usage(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, ws string, _ none) (usageResponse, error) {
		used, err := a.Quota.Usage(ctx, a.Store, ws)
		if err != nil {
			return usageResponse{}, err
		}
		limit, err := planLimit(ctx, ws)
		return usageResponse{UsedBytes: used.Bytes, UsedFiles: used.Files, LimitBytes: limit.Bytes, LimitFiles: limit.Files}, err
	})
}

// errorResponse is the JSON body of a failed request.
type errorResponse struct {
	Error string          `json:"error"`
	Code  filesystem.Code `json:"code"`
}

// writeError answers with the status for err's code (507 for a full
// workspace); server-side failures say only their status text.
// ds:def id=writeerror-3zdpmjfy
func writeError(w http.ResponseWriter, err error) {
	status := fshttp.StatusOf(err)
	writeJSON(w, status, errorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
}

// writeJSON writes v as the JSON body with status.
func writeJSON[T any](w http.ResponseWriter, status int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // a client that hung up cannot be told
}

func main() {
	// Ctrl+C or SIGTERM stops the server cleanly, so the deferred
	// clean-up below runs.
	ctx, stop := serve.Context()
	defer stop()
	dir, err := os.MkdirTemp("", "recipe-team-drive-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	app, err := New(ctx, dir, baseURL, bytes.Repeat([]byte("change-me"), 4))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("listening on", baseURL, "data in", dir)
	srv := &http.Server{Addr: listenAddr, Handler: app.Handler, ReadHeaderTimeout: readHeaderTimeout}
	if err := serve.UntilDone(ctx, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
