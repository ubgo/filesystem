// Command maintenance walks through the maintenance tools of
// github.com/ubgo/filesystem: a dry run before a sweep, an integrity
// scrub, a disk-to-disk migration, a sync between disks and a subject
// export with an expiring link, all on memory disks and SQLite.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/jobs"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Retention and lifetimes of this deployment.
const (
	trashRetention = 30 * 24 * time.Hour
	exportTTL      = 15 * time.Minute
	exportDir      = "exports"
)

// docs accepts the plain-text files this demo stores.
var docs = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"text/plain"}, Key: upload.RandomKey("files")}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// deployment is the demo's store over three disks: "hot" where files
// land, "cold" they migrate to, and "exports" that signs its links.
type deployment struct {
	hot, cold, exports *filesystem.Disk
	store              *records.Store
}

// previewTrash runs the trash sweep as a dry run first and prints what it
// would purge; the real run then purges exactly that.
// ds:def id=previewtrash-k2m7qx4n
func previewTrash(ctx context.Context, out io.Writer, store *records.Store, now time.Time) error {
	sweep := jobs.SweepTrash(store, trashRetention)
	plan, err := sweep.DryRun(ctx, now)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "dry run: would purge %d file(s)\n", plan.Count)
	done, err := sweep.Run(ctx, now)
	fmt.Fprintf(out, "sweep: purged %d file(s)\n", done.Count)
	return err
}

// scrub checks every object on hot against its row, reporting what is
// missing or changed and touching nothing.
// ds:def id=scrub-p9w3re6t
func scrub(ctx context.Context, out io.Writer, d deployment, now time.Time) error {
	job := jobs.Scrub(d.store, jobs.ScrubOptions{
		Disk:       "hot",
		Cursor:     jobs.ObjectCursor(d.cold, "_jobs/scrub-hot"),
		Interval:   10 * time.Millisecond, // at most 100 objects a second
		OnMissing:  func(f records.File) { fmt.Fprintf(out, "scrub: %s is missing\n", f.Filename) },
		OnMismatch: func(f records.File, _ int64, _ string) { fmt.Fprintf(out, "scrub: %s changed\n", f.Filename) },
	})
	res, err := job.Run(ctx, now)
	fmt.Fprintf(out, "scrub: checked %d file(s)\n", res.Count)
	return err
}

// migrate moves every file from hot to cold, resumably.
// ds:def id=migrate-b5n8ja2c
func migrate(ctx context.Context, out io.Writer, d deployment, now time.Time) error {
	job := jobs.MigrateDisk(d.store, "hot", "cold", jobs.MigrateOptions{
		Cursor:   jobs.ObjectCursor(d.cold, "_jobs/migrate-hot"),
		OnFailed: func(f records.File, err error) { fmt.Fprintf(out, "migrate: %s stays on hot: %v\n", f.Filename, err) },
	})
	res, err := job.Run(ctx, now)
	fmt.Fprintf(out, "migrate: moved %d file(s) to cold\n", res.Count)
	return err
}

// syncBackup mirrors cold's files/ to a backup disk, previewing first.
// ds:def id=syncbackup-r4t6vu8w
func syncBackup(ctx context.Context, out io.Writer, cold, backup *filesystem.Disk) error {
	plan, err := filesystem.Sync(ctx, cold, "files", backup, "files", filesystem.SyncOptions{Delete: true, DryRun: true})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "sync dry run: %d to copy, %d to delete\n", plan.Copied, plan.Deleted)
	rep, err := filesystem.Sync(ctx, cold, "files", backup, "files", filesystem.SyncOptions{Delete: true})
	fmt.Fprintf(out, "sync: copied %d, deleted %d, unchanged %d\n", rep.Copied, rep.Deleted, rep.Unchanged)
	return err
}

// exportUser bundles everything a user uploaded into a zip with a link
// that expires, and sweeps expired exports.
// ds:def id=exportuser-h3c9mz1d
func exportUser(ctx context.Context, out io.Writer, d deployment, user string, now time.Time) error {
	exp, err := jobs.ExportSubject(ctx, d.store, d.exports, jobs.Subject{Scope: "acme", UploadedBy: user}, jobs.ExportOptions{
		Dir: exportDir, TTL: exportTTL, Filename: user + "-export.zip",
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "export: %d file(s), %d bytes, link valid until %s\n", exp.Files, exp.Bytes, exp.ExpiresAt.Format(time.Kitchen))
	// Scheduled in production; here run once, after the link expired.
	swept, err := jobs.UploadSweep(d.exports, exportDir, exportTTL).Run(ctx, now.Add(exportTTL+time.Minute))
	fmt.Fprintf(out, "export sweep: deleted %d expired export(s)\n", swept.Count)
	return err
}

func run(ctx context.Context, out io.Writer) error {
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	d, cleanup, err := open(ctx, clock)
	if err != nil {
		return err
	}
	defer cleanup()

	// Three uploads by one user; one is trashed long ago, one is damaged
	// behind the Store's back.
	userCtx := records.WithActor(ctx, "user-1")
	var files []records.File
	for _, name := range []string{"notes.txt", "report.txt", "old.txt"} {
		f, err := d.store.Upload(userCtx, "acme", "hot", strings.NewReader("contents of "+name), name, docs)
		if err != nil {
			return err
		}
		files = append(files, f)
	}
	if err := d.store.Trash(ctx, "acme", files[2].ID); err != nil {
		return err
	}
	if _, err := d.hot.WriteBytes(ctx, files[1].Key, []byte("damaged!"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	later := now.Add(trashRetention + 24*time.Hour)

	if err := previewTrash(ctx, out, d.store, later); err != nil {
		return err
	}
	if err := scrub(ctx, out, d, later); err != nil {
		return err
	}
	// The damaged object would fail MoveFile's check; restore it first,
	// as an operator would from a backup.
	if _, err := d.hot.WriteBytes(ctx, files[1].Key, []byte("contents of report.txt"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	if err := migrate(ctx, out, d, later); err != nil {
		return err
	}
	backup, err := filesystem.NewDisk("backup", memory.New())
	if err != nil {
		return err
	}
	if _, err := backup.WriteBytes(ctx, "files/stale.txt", []byte("gone from cold"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	if err := syncBackup(ctx, out, d.cold, backup); err != nil {
		return err
	}
	return exportUser(userCtx, out, d, "user-1", now)
}

// open builds the deployment on memory disks and a temporary SQLite file.
func open(ctx context.Context, clock func() time.Time) (deployment, func(), error) {
	signer, err := signedurl.New("https://files.example.com/s", []signedurl.Key{{ID: "k1", Secret: []byte(strings.Repeat("demo-secret-", 3))}}, signedurl.WithClock(clock))
	if err != nil {
		return deployment{}, nil, err
	}
	var d deployment
	mk := func(name string, opts ...filesystem.Option) *filesystem.Disk {
		disk, mkErr := filesystem.NewDisk(name, memory.New(memory.WithClock(clock)), opts...)
		if mkErr != nil {
			err = mkErr
		}
		return disk
	}
	d.hot, d.cold, d.exports = mk("hot"), mk("cold"), mk("exports", filesystem.WithURLSigner(signer))
	if err != nil {
		return deployment{}, nil, err
	}
	mgr, err := filesystem.NewManager(d.hot, d.cold, d.exports)
	if err != nil {
		return deployment{}, nil, err
	}
	dir, err := os.MkdirTemp("", "maintenance-*")
	if err != nil {
		return deployment{}, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "db.sqlite")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		cleanup()
		return deployment{}, nil, err
	}
	db.SetMaxOpenConns(1)
	closeAll := func() { _ = db.Close(); cleanup() }
	if d.store, err = records.New(db, sqlb.SQLiteDialect{}, mgr, records.WithClock(clock)); err != nil {
		closeAll()
		return deployment{}, nil, err
	}
	if _, err := d.store.Migrate(ctx); err != nil {
		closeAll()
		return deployment{}, nil, err
	}
	return d, closeAll, nil
}
