// Command records-status shows the processing status of recorded files on
// SQLite and an in-memory disk: a file recorded as processing, a worker
// reporting progress and the outcome through Store.SetStatus, a plugin
// told of each change with the file in the event, the failed files listed
// with a status filter, and a client polling GET /files/{id}/status of
// recordshttp.
//
//	go run ./records-status
//
// It prints what each step returned; main_test.go runs the same steps.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordshttp"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	diskName = "media"
	tenant   = "acme"
	// ownerPost is the owner type the HTTP endpoints accept.
	ownerPost = "post"
)

// pngBytes is enough of a PNG for the disk to detect image/png.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-demo")

// statusLog is a plugin told of every status change, with the file as the
// change left it: where an app sends a webhook or a push notification.
// ds:def id=statuslog-5xm5mey6
type statusLog struct{ lines []string }

func (l *statusLog) Name() string { return "statuslog" }

func (l *statusLog) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	if e.Kind != records.EventFileStatusChanged {
		return nil
	}
	line := fmt.Sprintf("%s is %s", e.File.Key, e.File.Status)
	if e.File.StatusDetail != "" {
		line += " (" + e.File.StatusDetail + ")"
	}
	// After the commit: a notification must not announce a change that
	// rolled back.
	s.AfterCommit(ctx, func(context.Context) { l.lines = append(l.lines, line) })
	return nil
}

func newStore(ctx context.Context, db *sql.DB, log *statusLog) (*records.Store, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(ownerPost), records.WithPlugins(log))
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// recordForProcessing records a stored object as processing, so clients
// see from the start that its derived files are not there yet.
// ds:def id=recordforprocessing-p8xeh35t
func recordForProcessing(ctx context.Context, s *records.Store, key string) (records.File, error) {
	disk, err := s.Disks().Disk(diskName)
	if err != nil {
		return records.File{}, err
	}
	if _, err := disk.WriteBytes(ctx, key, pngBytes, filesystem.WriteOptions{}); err != nil {
		return records.File{}, err
	}
	return s.CreateFile(ctx, records.NewFile{
		Scope: tenant, Disk: diskName, Key: key,
		Status: records.StatusProcessing, StatusDetail: "queued",
	})
}

// process is the worker: it reports progress, then the outcome. A real
// worker makes thumbnails or runs a scan between the calls.
// ds:def id=process-jpk92ygd
func process(ctx context.Context, s *records.Store, f records.File, fail bool) error {
	if _, err := s.SetStatus(ctx, tenant, f.ID, records.StatusProcessing, "thumbnails 1/2"); err != nil {
		return err
	}
	if fail {
		_, err := s.SetStatus(ctx, tenant, f.ID, records.StatusFailed, "thumbnail: unsupported format")
		return err
	}
	_, err := s.SetStatus(ctx, tenant, f.ID, records.StatusReady, "")
	return err
}

// failedFiles lists the tenant's files whose processing failed.
// ds:def id=failedfiles-3z346hpy
func failedFiles(ctx context.Context, s *records.Store) ([]records.File, error) {
	page, err := s.Files(ctx, records.FileQuery{
		Scope:   tenant,
		Filters: []sqlb.Predicate{sqlb.Eq(records.FileStatus, records.StatusFailed)},
	})
	return page.Files, err
}

// poll asks the HTTP endpoints for a file's status, as a client does
// until it is ready or failed.
// ds:def id=poll-ghfthrat
func poll(srv http.Handler, fileID string) (recordshttp.StatusResponse, error) {
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/"+fileID+"/status", nil))
	var out recordshttp.StatusResponse
	if rec.Code != http.StatusOK {
		return out, fmt.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
	return out, json.Unmarshal(rec.Body.Bytes(), &out)
}

func run(ctx context.Context, w io.Writer, dir string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "records.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	events := &statusLog{}
	s, err := newStore(ctx, db, events)
	if err != nil {
		return err
	}
	srv, err := recordshttp.Handler(recordshttp.Config{
		Store: s, Disk: diskName, Policy: upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}},
		Scope:     func(*http.Request) (string, error) { return tenant, nil },
		Authorize: func(*http.Request, recordshttp.Action, recordshttp.Target) error { return nil }, // the demo allows everything
	})
	if err != nil {
		return err
	}

	good, err := recordForProcessing(ctx, s, "photos/good.png")
	if err != nil {
		return err
	}
	bad, err := recordForProcessing(ctx, s, "photos/bad.png")
	if err != nil {
		return err
	}
	before, err := poll(srv, good.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "poll before: %s (%s)\n", before.Status, before.StatusDetail)
	if err := process(ctx, s, good, false); err != nil {
		return err
	}
	if err := process(ctx, s, bad, true); err != nil {
		return err
	}
	for _, line := range events.lines {
		fmt.Fprintln(w, "event:", line)
	}
	after, err := poll(srv, good.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "poll after: %s\n", after.Status)
	failed, err := failedFiles(ctx, s)
	if err != nil {
		return err
	}
	for _, f := range failed {
		fmt.Fprintf(w, "failed: %s: %s\n", f.Key, f.StatusDetail)
	}
	return quarantineDemo(ctx, w, db)
}

func main() {
	dir, err := os.MkdirTemp("", "records-status")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
