// Command recipe-bulk-import is the "import your contacts from a CSV"
// feature of a CRM, a mailing tool or any SaaS onboarding flow: the upload
// is accepted in milliseconds and answered 202, a background job parses it
// later, the valid rows land in the app's own table in the same
// transaction that marks the import done, and the rows it refused come
// back as a downloadable report.
//
//	go run ./recipe-bulk-import       (listens on localhost:8122)
//
// Routes (the demo trusts the X-Tenant header; a real app uses its session):
//
//	POST /imports              multipart form, a CSV in field "file"; 202
//	GET  /imports/{id}         status: processing, ready or failed, and why
//	GET  /imports/{id}/report  the refused rows, as CSV
//	GET  /contacts             the tenant's imported contacts
package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/examples/internal/serve"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/jobs"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this app, not of the library.
const (
	// listenAddr is where the demo server listens.
	listenAddr = "localhost:8122"
	// diskName names the one disk in the manager.
	diskName = "imports"
	// headerTenant carries the demo's tenant; a real app reads its session.
	headerTenant = "X-Tenant"
	// maxImportBytes caps one CSV: about 100,000 contacts.
	maxImportBytes = 10 << 20
	// formOverhead is the multipart framing allowed on top of the file.
	formOverhead = 64 << 10
	// maxRows bounds one import, so a job run has a known worst case.
	maxRows = 100_000
	// importEvery is how often the job looks for queued imports.
	importEvery = 2 * time.Second
	// importJitter spreads the runs of several instances apart.
	importJitter = 500 * time.Millisecond
	// queueBatch is how many queued imports one query picks up.
	queueBatch = 10
	// jobName names the import job in reports and logs.
	jobName = "app.contact_import"
	// detailQueued is the status detail of an import nobody picked up yet.
	detailQueued = "queued"
	// reportPrefix is where the refused rows of each import are written.
	reportPrefix = "reports/"
	// readHeaderTimeout bounds a slow client's headers.
	readHeaderTimeout = 10 * time.Second
)

// header is the first line every import must have, in this order.
var header = []string{"name", "email"}

// importPolicy accepts CSV only, detected from the bytes and the .csv
// extension (plain text named .csv is text/csv), up to maxImportBytes,
// stored under a random key so the client's file name never reaches it.
// ds:def id=importpolicy-fn7wrfhw
var importPolicy = upload.Policy{
	MaxSize:    maxImportBytes,
	MinSize:    1,
	Allowed:    []string{"text/csv"},
	Extensions: []string{".csv"},
	Key:        upload.RandomKey("incoming"),
}

// contactsSchema is the app's own table, which the import fills.
const contactsSchema = `CREATE TABLE IF NOT EXISTS contacts (
	scope TEXT NOT NULL,
	email TEXT NOT NULL,
	name TEXT NOT NULL,
	import_id TEXT NOT NULL,
	PRIMARY KEY (scope, email)
)`

// App holds the wired dependencies.
type App struct {
	Store   *records.Store
	DB      *sql.DB
	Disk    *filesystem.Disk
	Handler http.Handler
}

// New wires the app over a data directory.
func New(ctx context.Context, dataDir string) (*App, error) {
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv)
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "app.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks)
	if err != nil {
		return nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, contactsSchema); err != nil {
		return nil, err
	}
	app := &App{Store: store, DB: db, Disk: disk}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /imports", app.accept)
	mux.HandleFunc("GET /imports/{id}", app.status)
	mux.HandleFunc("GET /imports/{id}/report", app.report)
	mux.HandleFunc("GET /contacts", app.contacts)
	app.Handler = mux
	return app, nil
}

// enqueue stores the CSV and records it as processing in one transaction,
// so no client ever sees it as ready before the job ran, and a failed
// SetStatus deletes the stored object with the rollback.
// ds:def id=app-enqueue-crnrjh47
func (a *App) enqueue(ctx context.Context, tenant string, body io.Reader, filename string) (records.File, error) {
	var f records.File
	err := a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		stored, err := a.Store.Upload(ctx, tenant, diskName, body, filename, importPolicy)
		if err != nil {
			return err
		}
		f, err = a.Store.SetStatus(ctx, tenant, stored.ID, records.StatusProcessing, detailQueued)
		return err
	})
	return f, err
}

// importResponse is what a client polls.
type importResponse struct {
	ImportID string         `json:"import_id"`
	Status   records.Status `json:"status"`
	Detail   string         `json:"detail,omitempty"`
}

// accept answers 202 as soon as the CSV is stored: parsing waits for the
// job, so a 100,000-row file never holds the request open.
// ds:def id=app-accept-q7zkg5cx
func (a *App) accept(w http.ResponseWriter, r *http.Request) {
	tenant := r.Header.Get(headerTenant)
	if tenant == "" {
		writeError(w, filesystem.Errorf(filesystem.CodePermission, "sign in first"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBytes+formOverhead)
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "send the CSV in the form field \"file\""))
		return
	}
	defer func() { _ = file.Close() }()
	f, err := a.enqueue(r.Context(), tenant, file, hdr.Filename)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Location", "/imports/"+f.ID)
	writeJSON(w, http.StatusAccepted, importResponse{ImportID: f.ID, Status: f.Status, Detail: f.StatusDetail})
}

// ImportJob is the background half: every run imports each queued CSV.
// It is safe to run again after a crash, which jobs require: an import is
// marked ready in the same transaction that inserts its rows, so a run
// that died half way left nothing behind and the next run starts that
// import over.
// ds:def id=app-importjob-24hs86qp
func (a *App) ImportJob() jobs.Job {
	return jobs.Func(jobName, func(ctx context.Context, _ time.Time) (jobs.Result, error) {
		var n int64
		for {
			queued, err := a.queued(ctx)
			if err != nil || len(queued) == 0 {
				return jobs.Result{Count: n}, err
			}
			for _, f := range queued {
				if err := a.importOne(ctx, f); err != nil {
					return jobs.Result{Count: n}, err
				}
				n++
			}
		}
	})
}

// queued finds imports still processing, across every tenant: Store.Files
// lists one scope, so the job queries the files table itself, with the
// typed columns records exports. The status column is the queue.
// ds:def id=app-queued-6s5xp9w4
func (a *App) queued(ctx context.Context) ([]records.File, error) {
	var scope, id string
	q := sqlb.NewSelect(a.Store.Dialect()).
		Columns(sqlb.Bind(records.FileScope, &scope), sqlb.Bind(records.FileID, &id)).
		From(records.TableFiles).
		Where(sqlb.Eq(records.FileStatus, records.StatusProcessing), sqlb.Eq(records.FileDisk, diskName), sqlb.IsNull(records.FileDeletedAt)).
		Order(sqlb.OrderBy(records.FileCreatedAt, sqlb.SortAsc)).Limit(queueBatch)
	var keys [][2]string
	if err := a.Store.Each(ctx, q, func() error {
		keys = append(keys, [2]string{scope, id})
		return nil
	}); err != nil {
		return nil, err
	}
	// Read the rows after the listing is closed: the query holds its
	// connection until then, and SQLite here has only one.
	out := make([]records.File, 0, len(keys))
	for _, k := range keys {
		f, err := a.Store.File(ctx, k[0], k[1])
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// errBadFile is an import the user must fix; anything else is the
// server's problem and is retried on the next run.
var errBadFile = errors.New("bad import file")

// importOne parses one CSV and commits its valid rows together with the
// ready status. A file the user must fix (wrong header, too many rows) is
// marked failed with the reason; the refused rows go to a report.
// ds:def id=app-importone-dvsv59a6
func (a *App) importOne(ctx context.Context, f records.File) error {
	rows, refused, err := a.parse(ctx, f)
	if errors.Is(err, errBadFile) {
		_, err = a.Store.SetStatus(ctx, f.Scope, f.ID, records.StatusFailed, strings.TrimPrefix(err.Error(), errBadFile.Error()+": "))
		return err
	}
	if err != nil {
		return err
	}
	if err := a.writeReport(ctx, f.ID, refused); err != nil {
		return err
	}
	return a.Store.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, c := range rows {
			if _, err := tx.ExecContext(ctx, `INSERT INTO contacts (scope, email, name, import_id) VALUES (?, ?, ?, ?)
				ON CONFLICT (scope, email) DO UPDATE SET name = excluded.name, import_id = excluded.import_id`, f.Scope, c[1], c[0], f.ID); err != nil {
				return err
			}
		}
		_, err := a.Store.SetStatus(ctx, f.Scope, f.ID, records.StatusReady, fmt.Sprintf("imported %d, refused %d", len(rows), len(refused)))
		return err
	})
}

// parse streams the stored CSV row by row; it never loads the file whole.
// It returns the valid rows and the refused ones, each refused row with
// its line number and reason prepended.
// ds:def id=app-parse-j59h3hyv
func (a *App) parse(ctx context.Context, f records.File) (rows, refused [][]string, err error) {
	rc, err := a.Disk.Read(ctx, f.Key)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rc.Close() }()
	cr := csv.NewReader(rc)
	cr.FieldsPerRecord = -1 // a short row is refused with a reason, not fatal
	first, err := cr.Read()
	if err != nil || len(first) != len(header) || !strings.EqualFold(strings.TrimSpace(first[0]), header[0]) || !strings.EqualFold(strings.TrimSpace(first[1]), header[1]) {
		return nil, nil, fmt.Errorf("%w: line 1 must be the header %q", errBadFile, strings.Join(header, ","))
	}
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return rows, refused, nil
		}
		if line-1 > maxRows {
			return nil, nil, fmt.Errorf("%w: more than %d rows", errBadFile, maxRows)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%w: line %d: not valid CSV", errBadFile, line)
		}
		if reason := checkRow(rec); reason != "" {
			refused = append(refused, append([]string{fmt.Sprint(line), reason}, rec...))
			continue
		}
		rows = append(rows, []string{strings.TrimSpace(rec[0]), strings.ToLower(strings.TrimSpace(rec[1]))})
	}
}

// checkRow returns why a row is refused, or "".
func checkRow(rec []string) string {
	if len(rec) != len(header) {
		return fmt.Sprintf("expected %d fields, got %d", len(header), len(rec))
	}
	if strings.TrimSpace(rec[0]) == "" {
		return "name is empty"
	}
	if _, err := mail.ParseAddress(strings.TrimSpace(rec[1])); err != nil {
		return "email is not an address"
	}
	return ""
}

// writeReport stores the refused rows as a CSV next to the import, with
// the line number and reason first. Written before the commit and keyed by
// the import's ID, so a run that is repeated overwrites it.
func (a *App) writeReport(ctx context.Context, importID string, refused [][]string) error {
	pr, pw := io.Pipe()
	go func() {
		cw := csv.NewWriter(pw)
		_ = cw.Write(append([]string{"line", "reason"}, header...))
		_ = cw.WriteAll(refused) // WriteAll flushes
		_ = pw.CloseWithError(cw.Error())
	}()
	_, err := a.Disk.Write(ctx, reportPrefix+importID+".csv", pr, filesystem.WriteOptions{ContentType: "text/csv; charset=utf-8"})
	_ = pr.CloseWithError(err)
	return err
}

// status answers the import's status for a client polling it.
func (a *App) status(w http.ResponseWriter, r *http.Request) {
	tenant := r.Header.Get(headerTenant)
	f, err := a.Store.File(r.Context(), tenant, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, importResponse{ImportID: f.ID, Status: f.Status, Detail: f.StatusDetail})
}

// report streams the refused rows of a finished import.
func (a *App) report(w http.ResponseWriter, r *http.Request) {
	tenant := r.Header.Get(headerTenant)
	f, err := a.Store.File(r.Context(), tenant, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if f.Status != records.StatusReady {
		writeError(w, filesystem.Errorf(filesystem.CodeNotFound, "import %s has no report while %s", f.ID, f.Status))
		return
	}
	rc, err := a.Disk.Read(r.Context(), reportPrefix+f.ID+".csv")
	if err != nil {
		writeError(w, err)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="refused-rows.csv"`)
	_, _ = io.Copy(w, rc) // a client that hung up cannot be told
}

// contact is one row of the app's table.
type contact struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// contacts lists the tenant's contacts.
func (a *App) contacts(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.QueryContext(r.Context(), `SELECT name, email FROM contacts WHERE scope = ? ORDER BY email`, r.Header.Get(headerTenant))
	if err != nil {
		writeError(w, err)
		return
	}
	defer func() { _ = rows.Close() }()
	out := []contact{}
	for rows.Next() {
		var c contact
		if err := rows.Scan(&c.Name, &c.Email); err != nil {
			writeError(w, err)
			return
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// writeError answers with the status for err's code and a JSON body that
// never carries a disk name or key.
func writeError(w http.ResponseWriter, err error) {
	writeJSON(w, fshttp.StatusOf(err), upload.ErrorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
}

// writeJSON writes v as the JSON body with status.
func writeJSON[T any](w http.ResponseWriter, status int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // a client that hung up cannot be told
}

func main() {
	// Ctrl+C or SIGTERM stops the server and the scheduler cleanly, so
	// the deferred clean-up below runs.
	ctx, stop := serve.Context()
	defer stop()
	dir, err := os.MkdirTemp("", "recipe-bulk-import-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	app, err := New(ctx, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	done := runJobs(ctx, app)
	fmt.Println("listening on", "http://"+listenAddr, "data in", dir)
	srv := &http.Server{Addr: listenAddr, Handler: app.Handler, ReadHeaderTimeout: readHeaderTimeout}
	if err := serve.UntilDone(ctx, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	<-done
}

// runJobs runs the import job every few seconds in this process until ctx
// ends, logging each run that did something or failed. The returned
// channel closes once the scheduler has stopped.
// ds:def id=runjobs-sptdz9dw
func runJobs(ctx context.Context, app *App) <-chan struct{} {
	sched := jobs.NewScheduler(jobs.SchedulerOptions{OnRun: func(r jobs.Report) {
		if r.Err != nil || r.Result.Count > 0 {
			fmt.Printf("job %s: %d imported, err=%v\n", r.Job, r.Result.Count, r.Err)
		}
	}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := sched.Run(ctx, jobs.Schedule{Job: app.ImportJob(), Every: importEvery, Jitter: importJitter, RunAtStart: true}); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}()
	return done
}
