// Command jobs-scheduler runs a deployment's maintenance jobs with the
// in-process scheduler of github.com/ubgo/filesystem/jobs: trash retention,
// abandoned uploads, orphaned objects and audit retention, each on its own
// interval, with every run reported to one callback.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/jobs"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/audit"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Retention periods of this deployment.
const (
	trashRetention = 30 * 24 * time.Hour
	uploadMaxAge   = 24 * time.Hour
	auditRetention = 90 * 24 * time.Hour
)

const diskName = "media"

// images stores recorded files under uploads/; the orphan sweep watches
// files/, where this demo leaves an object no file owns.
var images = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}, Key: upload.RandomKey("uploads")}

const pngData = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-png-body"

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// schedules is the maintenance of this deployment: every job, how often
// it runs, and how much jitter spreads several instances apart.
// ds:def id=schedules-3acmgv45
func schedules(store *records.Store, disk *filesystem.Disk, auditLog *audit.Plugin) []jobs.Schedule {
	return []jobs.Schedule{
		{Job: jobs.SweepTrash(store, trashRetention), Every: time.Hour, Jitter: 5 * time.Minute, RunAtStart: true},
		{Job: jobs.UploadSweep(disk, "incoming", uploadMaxAge), Every: time.Hour, Jitter: 5 * time.Minute, RunAtStart: true},
		{Job: jobs.SweepAbandoned(disk, uploadMaxAge), Every: 6 * time.Hour, Jitter: 10 * time.Minute, RunAtStart: true},
		{Job: jobs.SweepOrphans(store, diskName, "files", uploadMaxAge), Every: 24 * time.Hour, Jitter: time.Hour, RunAtStart: true},
		{Job: jobs.AuditPrune(auditLog, store, auditRetention), Every: 24 * time.Hour, Jitter: time.Hour, RunAtStart: true},
	}
}

// startScheduler runs the schedules until ctx ends, sending every report
// to report (here a log line; in production, also a failure counter).
// ds:def id=startscheduler-mcxjtq47
func startScheduler(ctx context.Context, all []jobs.Schedule, now func() time.Time, report func(jobs.Report)) <-chan error {
	s := jobs.NewScheduler(jobs.SchedulerOptions{OnRun: report, Now: now})
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, all...) }()
	return done
}

func run(ctx context.Context, out io.Writer) error {
	// The data is written at day 0; the scheduler then starts at day 60, so
	// every retention period has passed.
	day0 := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return day0 }
	disk, err := filesystem.NewDisk(diskName, memory.New(memory.WithClock(clock)))
	if err != nil {
		return err
	}
	disks, _ := filesystem.NewManager(disk)
	dir, err := os.MkdirTemp("", "jobs-scheduler-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "db.sqlite")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	auditLog := audit.New()
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithClock(clock), records.WithPlugins(auditLog))
	if err != nil {
		return err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return err
	}
	if err := seed(ctx, store, disk); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	var mu sync.Mutex
	var lines []string
	all := schedules(store, disk, auditLog)
	day60 := day0.Add(60 * 24 * time.Hour)
	done := startScheduler(ctx, all, func() time.Time { return day60 }, func(r jobs.Report) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf("%s: %d, err %v", r.Job, r.Result.Count, r.Err))
		if len(lines) == len(all) {
			cancel() // every job ran once at start; stop the demo
		}
	})
	if err := <-done; err != nil {
		cancel()
		return err
	}
	slices.Sort(lines)
	fmt.Fprintln(out, strings.Join(lines, "\n"))
	return nil
}

// seed leaves one of each kind of leftover: a trashed file, an upload
// nobody confirmed, an abandoned multipart upload and an orphaned object.
func seed(ctx context.Context, store *records.Store, disk *filesystem.Disk) error {
	f, err := store.Upload(ctx, "acme", diskName, strings.NewReader(pngData), "old.png", images)
	if err != nil {
		return err
	}
	if err := store.Trash(ctx, "acme", f.ID); err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "incoming/never-confirmed.png", []byte(pngData), filesystem.WriteOptions{}); err != nil {
		return err
	}
	if _, err := disk.CreateMultipart(ctx, "incoming/big.mov", filesystem.WriteOptions{}); err != nil {
		return err
	}
	_, err = disk.WriteBytes(ctx, "files/orphan.png", []byte(pngData), filesystem.WriteOptions{})
	return err
}
