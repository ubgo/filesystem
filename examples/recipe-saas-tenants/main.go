// Command recipe-saas-tenants is the file storage of a multi-tenant SaaS
// product: every tenant's files are rows of its own scope, stored under its
// own key prefix, charged to its own plan's quota, optionally on a bucket of
// its own, and removed in one call when the tenant leaves.
//
//	go run ./recipe-saas-tenants       (listens on localhost:8113)
//
// The disks come from the environment (STORAGE_DISKS, STORAGE_<NAME>_DSN);
// without it the demo uses two local directories.
//
// Routes (every one needs "Authorization: Bearer <api key>"):
//
//	POST   /files                      multipart form, field "file"
//	GET    /files                      the tenant's files, newest first
//	GET    /files/{id}                 302 to a signed link
//	PUT    /settings/{name}            raw JSON on the tenant's scoped disk
//	GET    /settings/{name}
//	GET    /usage                      bytes and files against the plan
//	DELETE /admin/tenants/{id}         offboard a tenant (admin key only)
//	GET    /dl/…                       signed links, served by fshttp
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
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/s3"
	"github.com/ubgo/filesystem/examples/internal/serve"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this app, not of the library.
const (
	// listenAddr is where the demo server listens.
	listenAddr = "localhost:8113"
	// baseURL is the server's public origin; signed links start with it.
	baseURL = "http://" + listenAddr
	// signedPrefix is where fshttp.Signed serves signed links.
	signedPrefix = "/dl"
	// envPrefix starts every storage variable: STORAGE_DISKS,
	// STORAGE_FILES_DSN, STORAGE_TENANT_GLOBEX_DSN.
	envPrefix = "STORAGE_"
	// sharedDisk is the disk every tenant without a bucket of its own uses.
	sharedDisk = "files"
	// dedicatedPrefix names a tenant's own disk: "tenant-<id>".
	dedicatedPrefix = "tenant-"
	// keyRoot is the key directory each tenant's prefix lives under.
	keyRoot = "tenants"
	// maxFileSize caps one upload.
	maxFileSize = 10 << 20
	// maxSettingsSize caps one settings document.
	maxSettingsSize = 64 << 10
	// linkLifetime is how long a download link works.
	linkLifetime = 5 * time.Minute
	// listLimit is the page size of GET /files.
	listLimit = 50
	// readHeaderTimeout bounds a slow client's headers.
	readHeaderTimeout = 10 * time.Second
)

// Plan is a billing plan; its limits become the tenant's quota.
type Plan string

// The closed set of plans.
const (
	PlanFree Plan = "free"
	PlanPro  Plan = "pro"
)

// planLimits are the quotas of each plan. Zero in a field is unlimited.
// ds:def id=planlimits-wfzvqwrx
var planLimits = map[Plan]quota.Limit{
	PlanFree: {Bytes: 1 << 20, Files: 3},
	PlanPro:  {Bytes: 1 << 30},
}

// Principal is who a request acts for: a tenant member, or the operator.
type Principal struct {
	Tenant string
	Admin  bool
}

// Tenant is a customer account, as the app's own tables would hold it.
type Tenant struct {
	ID   string
	Plan Plan
}

// Directory stands in for the app's accounts tables and its API keys.
type Directory struct {
	Tenants map[string]Tenant
	Keys    map[string]Principal
}

// demoDirectory has two tenants, acme on the free plan and globex on pro,
// and an operator key.
func demoDirectory() Directory {
	return Directory{
		Tenants: map[string]Tenant{"acme": {ID: "acme", Plan: PlanFree}, "globex": {ID: "globex", Plan: PlanPro}},
		Keys: map[string]Principal{
			"key-acme":   {Tenant: "acme"},
			"key-globex": {Tenant: "globex"},
			"key-admin":  {Admin: true},
		},
	}
}

// demoEnv is the storage configuration used when STORAGE_DISKS is unset:
// a shared directory, and a directory of its own for globex. In production
// these are DSNs such as s3://acme-files?region=… in the environment.
// ds:def id=demoenv-t3e4nky5
func demoEnv(dataDir string) filesystem.LookupFunc {
	env := map[string]string{
		envPrefix + filesystem.EnvDisks:                         sharedDisk + "," + dedicatedPrefix + "globex",
		filesystem.EnvName(envPrefix, sharedDisk):               "file:" + filepath.Join(dataDir, "shared"),
		filesystem.EnvName(envPrefix, dedicatedPrefix+"globex"): "file:" + filepath.Join(dataDir, "globex"),
	}
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

// openDisks builds every disk from the environment. The Mux knows the
// schemes this app registers, so the same code opens local directories in
// development and S3 buckets in production. WithURLSigner applies to every
// disk; a backend that signs its own URLs (S3) uses its own signing instead.
// ds:def id=opendisks-jkxkrcu6
func openDisks(ctx context.Context, lookup filesystem.LookupFunc, signer *signedurl.Signer) (*filesystem.Manager, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(local.Scheme, local.Opener); err != nil {
		return nil, err
	}
	if err := mux.Register(s3.Scheme, s3.Opener); err != nil {
		return nil, err
	}
	return mux.OpenManager(ctx, envPrefix, lookup, filesystem.WithURLSigner(signer))
}

// newQuota charges every tenant against its plan. The plan is read when a
// file is recorded, so an upgrade applies to the next upload.
// ds:def id=newquota-2stc2hye
func newQuota(dir Directory) (*quota.Plugin, error) {
	return quota.New(func(_ context.Context, scope string) (quota.Limit, error) {
		t, ok := dir.Tenants[scope]
		if !ok {
			return quota.Limit{}, filesystem.Errorf(filesystem.CodePermission, "unknown tenant %q", scope)
		}
		return planLimits[t.Plan], nil
	})
}

// App holds the wired dependencies.
type App struct {
	Store   *records.Store
	Disks   *filesystem.Manager
	Quota   *quota.Plugin
	Dir     Directory
	Handler http.Handler
}

// New wires the app. lookup supplies the storage configuration.
func New(ctx context.Context, dataDir, publicBase string, secret []byte, lookup filesystem.LookupFunc) (*App, error) {
	signer, err := signedurl.New(publicBase+signedPrefix, []signedurl.Key{{ID: "k1", Secret: secret}})
	if err != nil {
		return nil, err
	}
	disks, err := openDisks(ctx, lookup, signer)
	if err != nil {
		return nil, err
	}
	app := &App{Disks: disks, Dir: demoDirectory()}
	if app.Quota, err = newQuota(app.Dir); err != nil {
		return nil, err
	}
	if app.Store, err = openStore(ctx, dataDir, disks, app.Quota); err != nil {
		return nil, err
	}
	app.Handler = app.routes(signer)
	return app, nil
}

// openStore opens SQLite and the records Store with the quota plugin.
func openStore(ctx context.Context, dataDir string, disks *filesystem.Manager, q *quota.Plugin) (*records.Store, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "app.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(q))
	if err != nil {
		return nil, err
	}
	_, err = store.Migrate(ctx)
	return store, err
}

// routes mounts the tenant API behind authenticate, and the signed links
// outside it: a signed link is its own authorization.
func (a *App) routes(signer *signedurl.Signer) http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("POST /files", a.uploadFile)
	api.HandleFunc("GET /files", a.listFiles)
	api.HandleFunc("GET /files/{id}", a.download)
	api.HandleFunc("PUT /settings/{name}", a.putSettings)
	api.HandleFunc("GET /settings/{name}", a.getSettings)
	api.HandleFunc("GET /usage", a.usage)
	api.HandleFunc("DELETE /admin/tenants/{id}", a.offboard)
	mux := http.NewServeMux()
	mux.Handle(signedPrefix+"/", http.StripPrefix(signedPrefix, fshttp.Signed(a.Disks, signer)))
	mux.Handle("/", a.authenticate(api))
	return mux
}

// principalKey is the context key of the request's Principal.
type principalKey struct{}

// authenticate resolves the bearer key to a Principal and puts it in the
// context. The tenant comes from the credential, never from the URL or a
// header the client chooses, so no request can name another tenant.
// ds:def id=app-authenticate-m82ras65
func (a *App) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.Dir.Keys[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			writeError(w, filesystem.Errorf(filesystem.CodePermission, "unknown API key"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

// tenantOf returns the tenant of a member request; operator keys have none.
func tenantOf(r *http.Request) (string, error) {
	p, _ := r.Context().Value(principalKey{}).(Principal)
	if p.Tenant == "" {
		return "", filesystem.Errorf(filesystem.CodePermission, "this key acts for no tenant")
	}
	return p.Tenant, nil
}

// diskFor returns the disk of a tenant: its own when the configuration has
// one ("tenant-<id>"), else the shared one.
// ds:def id=app-diskfor-puec8wbc
func (a *App) diskFor(tenant string) string {
	if _, err := a.Disks.Disk(dedicatedPrefix + tenant); err == nil {
		return dedicatedPrefix + tenant
	}
	return sharedDisk
}

// prefixOf is the key directory of a tenant on any disk.
func prefixOf(tenant string) string { return keyRoot + "/" + tenant }

// policyFor is the upload policy of one tenant: the same rules for all,
// keys under the tenant's own prefix, so its objects can be listed, copied
// or deleted as one directory.
// ds:def id=policyfor-p2nyj2hu
func policyFor(tenant string) upload.Policy {
	return upload.Policy{
		MaxSize: maxFileSize,
		Allowed: []string{"image/*", "application/pdf", "text/plain", "text/csv"},
		Key:     upload.RandomKey(prefixOf(tenant) + "/files"),
	}
}

// fileResponse is the JSON shape of one file.
type fileResponse struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Disk        string `json:"disk"`
	Key         string `json:"key"`
}

// toResponse converts a File for JSON.
func toResponse(f records.File) fileResponse {
	return fileResponse{ID: f.ID, Filename: f.Filename, ContentType: f.ContentType, Size: f.Size, Disk: f.Disk, Key: f.Key}
}

// uploadFile stores a file in the request's tenant: its scope, its disk,
// its key prefix. The quota plugin charges it in the same transaction and
// refuses it with CodeQuotaExceeded (507) past the plan's limit.
// ds:def id=app-uploadfile-v7gas2px
func (a *App) uploadFile(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenantOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFileSize+64<<10)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "send the file in the form field \"file\""))
		return
	}
	defer func() { _ = file.Close() }()
	f, err := a.Store.Upload(r.Context(), tenant, a.diskFor(tenant), file, header.Filename, policyFor(tenant))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toResponse(f))
}

// listFiles lists the tenant's files. The scope is the tenant, so no
// other tenant's row can appear, whatever the query string says.
func (a *App) listFiles(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenantOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	page, err := a.Store.Files(r.Context(), records.FileQuery{Scope: tenant, Limit: listLimit, Cursor: r.URL.Query().Get("cursor")})
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]fileResponse, 0, len(page.Files))
	for _, f := range page.Files {
		out = append(out, toResponse(f))
	}
	writeJSON(w, http.StatusOK, out)
}

// download redirects to a signed link. Store.File filters by scope, so
// another tenant's file ID is CodeNotFound: the answer does not even
// reveal that the ID exists.
// ds:def id=app-download-8j5huwey
func (a *App) download(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenantOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	f, err := a.Store.File(r.Context(), tenant, r.PathValue("id"))
	if err == nil && f.DeletedAt != nil {
		err = filesystem.Errorf(filesystem.CodeNotFound, "file is in the trash")
	}
	if err != nil {
		writeError(w, err)
		return
	}
	link, err := a.Store.URL(r.Context(), f, linkLifetime)
	if err != nil {
		writeError(w, err)
		return
	}
	http.Redirect(w, r, link, http.StatusFound)
}

// tenantDisk is a tenant's view of its disk: key "settings/x.json" is
// "tenants/<id>/settings/x.json" underneath, and keys are cleaned before
// the prefix is added, so "../other/…" cannot climb out of it.
// ds:def id=app-tenantdisk-6tdetehg
func (a *App) tenantDisk(tenant string) (*filesystem.Disk, error) {
	disk, err := a.Disks.Disk(a.diskFor(tenant))
	if err != nil {
		return nil, err
	}
	return disk.Scope(disk.Name()+"."+tenant, prefixOf(tenant))
}

// settingsKey is where a named settings document lives on the tenant disk.
func settingsKey(name string) string { return "settings/" + name + ".json" }

// putSettings writes raw bytes, no records row, through the tenant disk:
// for app data that is not a user file (configuration, generated assets).
// ds:def id=app-putsettings-tcq5z7vn
func (a *App) putSettings(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenantOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	disk, err := a.tenantDisk(tenant)
	if err != nil {
		writeError(w, err)
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxSettingsSize)
	e, err := disk.Write(r.Context(), settingsKey(r.PathValue("name")), body, filesystem.WriteOptions{ContentType: "application/json", MaxSize: maxSettingsSize})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": e.Key})
}

// getSettings reads a settings document back through the tenant disk.
func (a *App) getSettings(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenantOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	disk, err := a.tenantDisk(tenant)
	if err != nil {
		writeError(w, err)
		return
	}
	data, err := disk.ReadAll(r.Context(), settingsKey(r.PathValue("name")))
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.Copy(w, bytes.NewReader(data))
}

// usageResponse is the tenant's usage against its plan.
type usageResponse struct {
	Plan       Plan  `json:"plan"`
	Bytes      int64 `json:"bytes"`
	Files      int64 `json:"files"`
	LimitBytes int64 `json:"limit_bytes"`
	LimitFiles int64 `json:"limit_files"`
}

// usage reports what the tenant stores, for a billing page.
func (a *App) usage(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenantOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	u, err := a.Quota.Usage(r.Context(), a.Store, tenant)
	if err != nil {
		writeError(w, err)
		return
	}
	plan := a.Dir.Tenants[tenant].Plan
	limit := planLimits[plan]
	writeJSON(w, http.StatusOK, usageResponse{Plan: plan, Bytes: u.Bytes, Files: u.Files, LimitBytes: limit.Bytes, LimitFiles: limit.Files})
}

// purgeFiles purges every file of a tenant's scope, live and trashed, and
// returns how many. Each Purge deletes the rows and releases the quota in
// one transaction, then the object. The first page is fetched again after
// each pass because purged files leave the listing.
// ds:def id=app-purgefiles-ee7ughab
func (a *App) purgeFiles(ctx context.Context, tenant string) (int, error) {
	purged := 0
	for _, trashed := range []bool{false, true} {
		for {
			page, err := a.Store.Files(ctx, records.FileQuery{Scope: tenant, Trashed: trashed})
			if err != nil {
				return purged, err
			}
			if len(page.Files) == 0 {
				break
			}
			for _, f := range page.Files {
				if err := a.Store.Purge(ctx, tenant, f.ID); err != nil {
					return purged, err
				}
				purged++
			}
		}
	}
	return purged, nil
}

// offboardResponse reports what an offboarding removed.
type offboardResponse struct {
	Tenant string `json:"tenant"`
	Purged int    `json:"files_purged"`
}

// offboard removes everything a tenant stored: its records (rows,
// attachments, plugin rows, quota usage) through Purge, then whatever is
// left under its key prefix (raw objects such as settings, and orphans of
// failed deletions) with one DeletePrefix. Operator keys only. Rerunning it
// after a failure continues where it stopped.
// ds:def id=app-offboard-htwpfehk
func (a *App) offboard(w http.ResponseWriter, r *http.Request) {
	if p, _ := r.Context().Value(principalKey{}).(Principal); !p.Admin {
		writeError(w, filesystem.Errorf(filesystem.CodePermission, "operator key required"))
		return
	}
	tenant := r.PathValue("id")
	if _, ok := a.Dir.Tenants[tenant]; !ok {
		writeError(w, filesystem.Errorf(filesystem.CodeNotFound, "unknown tenant %q", tenant))
		return
	}
	n, err := a.purgeFiles(r.Context(), tenant)
	if err != nil {
		writeError(w, err)
		return
	}
	disk, err := a.Disks.Disk(a.diskFor(tenant))
	if err == nil {
		err = disk.DeletePrefix(r.Context(), prefixOf(tenant))
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, offboardResponse{Tenant: tenant, Purged: n})
}

// writeError answers with the status for err's code and a JSON body that
// names the code. Messages of server failures stay in the logs, except a
// quota refusal, which the tenant must be able to read.
func writeError(w http.ResponseWriter, err error) {
	status := fshttp.StatusOf(err)
	writeJSON(w, status, upload.ErrorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
}

// writeJSON writes v as the JSON body with status.
func writeJSON[T any](w http.ResponseWriter, status int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // a client that hung up cannot be told
}

// lookupEnv reads the real environment when STORAGE_DISKS is set, else the
// demo configuration.
func lookupEnv(dataDir string) filesystem.LookupFunc {
	if _, ok := os.LookupEnv(envPrefix + filesystem.EnvDisks); ok {
		return os.LookupEnv
	}
	return demoEnv(dataDir)
}

func main() {
	// Ctrl+C or SIGTERM stops the server cleanly, so the deferred
	// clean-up below runs.
	ctx, stop := serve.Context()
	defer stop()
	dir, err := os.MkdirTemp("", "recipe-saas-tenants-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	app, err := New(ctx, dir, baseURL, bytes.Repeat([]byte("change-me"), 4), lookupEnv(dir))
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
