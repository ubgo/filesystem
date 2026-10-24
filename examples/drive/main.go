// Command drive is the shape of a team drive built from the records
// plugins: workspaces (scopes) with a storage quota, files attached to
// projects through recordshttp, folders, tags, version history, share
// links, an activity feed, and image dimensions, all on one Store.
//
//	go run ./drive [-addr localhost:8080]
//
// Authentication is out of scope: the demo trusts the X-Workspace and
// X-User headers, which a real app replaces with its session lookup in
// identify. Everything else is the real wiring.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging/dimensions"
	"github.com/ubgo/filesystem/examples/internal/serve"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/audit"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/filesystem/records/recordshttp"
	"github.com/ubgo/filesystem/records/share"
	"github.com/ubgo/filesystem/records/tags"
	"github.com/ubgo/filesystem/records/versions"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this app, not of the library.
const (
	diskName       = "drive"
	ownerProject   = "project"
	headerSpace    = "X-Workspace"
	headerUser     = "X-User"
	workspaceBytes = 50 << 20 // every workspace's quota
	maxFileSize    = 10 << 20
	keptVersions   = 5
	maxShareTTL    = 7 * 24 * time.Hour
	maxJSONBody    = 64 << 10
)

// policy accepts documents and images, never active content.
var policy = upload.Policy{
	MaxSize: maxFileSize,
	Allowed: []string{"image/*", "application/pdf", "text/plain", "text/csv", "application/zip"},
	Key:     upload.RandomKey("files"),
}

// App holds the wired dependencies; tests reach the plugins through it.
type App struct {
	Store    *records.Store
	Folders  *folders.Plugin
	Tags     *tags.Plugin
	Versions *versions.Plugin
	Shares   *share.Plugin
	Audit    *audit.Plugin
	Quota    *quota.Plugin
	Handler  http.Handler
}

// New wires the app over a data directory and the server's public base URL.
// ds:def id=new-pd9xke6p
func New(ctx context.Context, dataDir, baseURL string, secret []byte) (*App, error) {
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, err
	}
	signer, err := signedurl.New(baseURL+"/files", []signedurl.Key{{ID: "k1", Secret: secret}})
	if err != nil {
		return nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv, filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "drive.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	q, err := quota.New(func(context.Context, string) (quota.Limit, error) { return quota.Limit{Bytes: workspaceBytes}, nil })
	if err != nil {
		return nil, err
	}
	v, err := versions.New(versions.WithKeep(keptVersions))
	if err != nil {
		return nil, err
	}
	app := &App{Folders: folders.New(), Tags: tags.New(), Versions: v, Shares: share.New(), Audit: audit.New(), Quota: q}
	app.Store, err = records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(ownerProject),
		records.WithPlugins(app.Quota, app.Versions, app.Folders, app.Tags, app.Shares, app.Audit, dimensions.New()))
	if err != nil {
		return nil, err
	}
	if _, err := app.Store.Migrate(ctx); err != nil {
		return nil, err
	}
	files, err := recordshttp.Handler(recordshttp.Config{
		Store: app.Store, Disk: diskName, Policy: policy,
		Scope: func(r *http.Request) (string, error) { return identify(r) },
		// Every member of a workspace may do everything in it; a real app
		// checks roles or project membership here.
		Authorize: func(*http.Request, recordshttp.Action, recordshttp.Target) error { return nil },
	})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", http.StripPrefix("/api", files))
	mux.HandleFunc("POST /api/folders", app.createFolder)
	mux.HandleFunc("PUT /api/files/{id}/folder", app.moveFile)
	mux.HandleFunc("PUT /api/files/{id}/tags", app.setTags)
	mux.HandleFunc("GET /api/files/{id}/versions", app.listVersions)
	mux.HandleFunc("POST /api/versions/{id}/restore", app.restoreVersion)
	mux.HandleFunc("POST /api/files/{id}/share", app.shareFile)
	mux.HandleFunc("GET /api/activity", app.activity)
	mux.HandleFunc("GET /api/usage", app.usage)
	mux.Handle("/s/", http.StripPrefix("/s/", share.Handler(app.Store, app.Shares)))
	mux.Handle("/files/", http.StripPrefix("/files", fshttp.Signed(disks, signer)))
	app.Handler = withActor(mux)
	return app, nil
}

var errNoSession = errors.New("no workspace")

// identify returns the request's workspace. Demo only: it trusts a header.
// ds:def id=identify-6g4cctkk
func identify(r *http.Request) (string, error) {
	if ws := r.Header.Get(headerSpace); ws != "" {
		return ws, nil
	}
	return "", errNoSession
}

// withActor puts the signed-in user in the context, where the audit
// plugin reads it for every change the request makes.
// ds:def id=withactor-jp5agfxx
func withActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user := r.Header.Get(headerUser); user != "" {
			r = r.WithContext(audit.WithActor(r.Context(), "user:"+user))
		}
		next.ServeHTTP(w, r)
	})
}

// handle decodes a JSON body into In (when it has fields), runs fn in the
// request's workspace and answers with its result or its error's status.
// ds:def id=handle-6uxq3fmz
func handle[In, Out any](w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, scope string, in In) (Out, error)) {
	scope, err := identify(r)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	var in In
	if r.ContentLength != 0 && r.Method != http.MethodGet {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&in); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	}
	out, err := fn(r.Context(), scope, in)
	if err != nil {
		status := fshttp.StatusOf(err)
		http.Error(w, http.StatusText(status), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

type folderRequest struct {
	Parent string `json:"parent"`
	Name   string `json:"name"`
}

// folderJSON is a folder as this API answers it: snake_case, like
// recordshttp, never the plugin's Go struct.
type folderJSON struct {
	ID        string    `json:"id"`
	ParentID  string    `json:"parent_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

func (a *App) createFolder(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, scope string, in folderRequest) (folderJSON, error) {
		f, err := a.Folders.Create(ctx, a.Store, scope, in.Parent, in.Name)
		return folderJSON{ID: f.ID, ParentID: f.ParentID, Name: f.Name, CreatedAt: f.CreatedAt}, err
	})
}

type moveRequest struct {
	Folder string `json:"folder"`
}

func (a *App) moveFile(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, scope string, in moveRequest) (moveRequest, error) {
		return in, a.Folders.Put(ctx, a.Store, scope, r.PathValue("id"), in.Folder)
	})
}

type tagsRequest struct {
	Tags []string `json:"tags"`
}

func (a *App) setTags(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, scope string, in tagsRequest) (tagsRequest, error) {
		stored, err := a.Tags.Set(ctx, a.Store, scope, r.PathValue("id"), in.Tags...)
		return tagsRequest{Tags: stored}, err
	})
}

type none struct{}

// versionJSON is a kept version as this API answers it.
type versionJSON struct {
	ID          string    `json:"id"`
	Number      int64     `json:"number"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	ReplacedAt  time.Time `json:"replaced_at"`
}

func (a *App) listVersions(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, scope string, _ none) ([]versionJSON, error) {
		if _, err := a.Store.File(ctx, scope, r.PathValue("id")); err != nil {
			return nil, err
		}
		list, err := a.Versions.Versions(ctx, a.Store, scope, r.PathValue("id"))
		out := make([]versionJSON, len(list))
		for i, v := range list {
			out[i] = versionJSON{ID: v.ID, Number: v.Number, Filename: v.Filename, ContentType: v.ContentType, Size: v.Size, ReplacedAt: v.ReplacedAt}
		}
		return out, err
	})
}

// fileJSON is a file as this API answers it, with its image dimensions
// when the dimensions plugin recorded them.
type fileJSON struct {
	FileID      string `json:"file_id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	UploadedBy  string `json:"uploaded_by,omitempty"`
}

func toFileJSON(f records.File) fileJSON {
	w, h, _ := dimensions.Of(f)
	return fileJSON{FileID: f.ID, Filename: f.Filename, ContentType: f.ContentType, Size: f.Size, Width: w, Height: h, UploadedBy: f.UploadedBy}
}

func (a *App) restoreVersion(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, scope string, _ none) (fileJSON, error) {
		f, err := a.Versions.Restore(ctx, a.Store, scope, r.PathValue("id"))
		return toFileJSON(f), err
	})
}

type shareRequest struct {
	TTLSeconds   int64 `json:"ttl_seconds"`
	MaxDownloads int64 `json:"max_downloads"`
}

type shareResponse struct {
	Path      string    `json:"path"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ds:def id=app-sharefile-rn3zppyb
func (a *App) shareFile(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, scope string, in shareRequest) (shareResponse, error) {
		ttl := time.Duration(in.TTLSeconds) * time.Second
		if ttl <= 0 || ttl > maxShareTTL {
			return shareResponse{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "ttl_seconds must be in (0, %d]", int64(maxShareTTL/time.Second))
		}
		sh, err := a.Shares.Create(ctx, a.Store, scope, r.PathValue("id"), share.Options{TTL: ttl, MaxDownloads: in.MaxDownloads})
		return shareResponse{Path: "/s/" + sh.Token, ExpiresAt: sh.ExpiresAt}, err
	})
}

// entryJSON is one line of the activity feed.
type entryJSON struct {
	Actor  string `json:"actor"`
	Kind   string `json:"kind"`
	FileID string `json:"file_id,omitempty"`
	// Subject is the folder or share link a plugin event is about.
	Subject string `json:"subject,omitempty"`
	// Detail is the plugin event's particulars (a tag, a new parent).
	Detail map[string]string `json:"detail,omitempty"`
	At     time.Time         `json:"at"`
}

// activityJSON is a page of the feed, newest first.
type activityJSON struct {
	Entries    []entryJSON `json:"entries"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

func (a *App) activity(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, scope string, _ none) (activityJSON, error) {
		page, err := a.Audit.Entries(ctx, a.Store, audit.Query{Scope: scope, Cursor: r.URL.Query().Get("cursor")})
		out := activityJSON{Entries: make([]entryJSON, len(page.Entries)), NextCursor: page.NextCursor}
		for i, e := range page.Entries {
			out.Entries[i] = entryJSON{Actor: e.Actor, Kind: string(e.Kind), FileID: e.FileID, Subject: e.Subject, Detail: e.Detail, At: e.At}
		}
		return out, err
	})
}

// usageJSON is what the workspace stores.
type usageJSON struct {
	Bytes int64 `json:"bytes"`
	Files int64 `json:"files"`
}

func (a *App) usage(w http.ResponseWriter, r *http.Request) {
	handle(w, r, func(ctx context.Context, scope string, _ none) (usageJSON, error) {
		u, err := a.Quota.Usage(ctx, a.Store, scope)
		return usageJSON{Bytes: u.Bytes, Files: u.Files}, err
	})
}

// defaultAddr is where the server listens unless -addr says otherwise.
const defaultAddr = "localhost:8080"

func main() {
	// Ctrl+C or SIGTERM stops the server cleanly, so the deferred
	// clean-up below runs.
	ctx, stop := serve.Context()
	defer stop()
	addr := flag.String("addr", defaultAddr, "listen address; the public URL is http://<addr>")
	flag.Parse()
	dir, err := os.MkdirTemp("", "drive-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	app, err := New(ctx, dir, "http://"+*addr, bytes.Repeat([]byte("change-me"), 4))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("listening on", *addr, "; data in", dir)
	srv := &http.Server{Addr: *addr, Handler: app.Handler, ReadHeaderTimeout: 10 * time.Second}
	if err := serve.UntilDone(ctx, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
