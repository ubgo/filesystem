package jobs_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/jobs"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/audit"
	"github.com/ubgo/filesystem/records/locks"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/records/share"
	"github.com/ubgo/filesystem/upload"
)

const pngData = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-png-body"

var images = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}}

// env is a migrated store over a memory disk "media" whose clock, and the
// plugins', is env.now.
type env struct {
	now   time.Time
	disk  *filesystem.Disk
	store *records.Store
	audit *audit.Plugin
	share *share.Plugin
	locks *locks.Plugin
	quota *quota.Plugin
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	clock := func() time.Time { return e.now }
	disk, err := filesystem.NewDisk("media", memory.New(memory.WithClock(clock)))
	if err != nil {
		t.Fatal(err)
	}
	e.disk = disk
	mgr, _ := filesystem.NewManager(disk)
	e.audit = audit.New()
	e.share = share.New(share.WithClock(clock))
	e.locks = locks.New(locks.WithClock(clock))
	e.quota, err = quota.New(func(context.Context, string) (quota.Limit, error) { return quota.Limit{Bytes: 1 << 30}, nil })
	if err != nil {
		t.Fatal(err)
	}
	db, dialect := recordstest.Open(t)
	e.store, err = records.New(db, dialect, mgr, records.WithClock(clock), records.WithPlugins(e.audit, e.share, e.quota, e.locks))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) upload(t *testing.T, scope string) records.File {
	t.Helper()
	f, err := e.store.Upload(t.Context(), scope, "media", strings.NewReader(pngData), "a.png", images)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func run(t *testing.T, j jobs.Job, now time.Time) (jobs.Result, error) {
	t.Helper()
	return j.Run(t.Context(), now)
}

func TestRetentionJobs(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	start := e.now
	trashed := e.upload(t, "s1")
	if err := e.store.Trash(ctx, "s1", trashed.ID); err != nil {
		t.Fatal(err)
	}
	link, err := e.share.Create(ctx, e.store, "s1", e.upload(t, "s1").ID, share.Options{TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	_ = link
	_, _ = e.disk.WriteBytes(ctx, "incoming/abandoned.png", []byte(pngData), filesystem.WriteOptions{})
	if _, err := e.disk.CreateMultipart(ctx, "big.bin", filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	later := start.Add(48 * time.Hour)
	for _, c := range []struct {
		job  jobs.Job
		name string
		want int64
	}{
		{jobs.SweepTrash(e.store, 24*time.Hour), jobs.NameSweepTrash, 1},
		{jobs.UploadSweep(e.disk, "incoming", 24*time.Hour), jobs.NameUploadSweep, 1},
		{jobs.SweepAbandoned(e.disk, 24*time.Hour), jobs.NameSweepAbandoned, 1},
		{jobs.SharePrune(e.share, e.store, 24*time.Hour), jobs.NameSharePrune, 1},
		{jobs.SweepOrphans(e.store, "media", "orphans", 24*time.Hour), jobs.NameSweepOrphans, 0},
	} {
		if c.job.Name != c.name {
			t.Errorf("name %q, want %q", c.job.Name, c.name)
		}
		// Not yet old enough: nothing happens.
		if res, err := run(t, c.job, start.Add(time.Hour)); err != nil || res.Count != 0 {
			t.Errorf("%s early: %+v, %v", c.name, res, err)
		}
		if res, err := run(t, c.job, later); err != nil || res.Count != c.want {
			t.Errorf("%s: %+v, %v; want %d", c.name, res, err, c.want)
		}
		// A second run finds nothing left.
		if res, err := run(t, c.job, later); err != nil || res.Count != 0 {
			t.Errorf("%s again: %+v, %v", c.name, res, err)
		}
	}
	// Audit entries were written by the uploads and the trash above.
	if res, err := run(t, jobs.AuditPrune(e.audit, e.store, time.Hour), later); err != nil || res.Count == 0 {
		t.Errorf("audit prune: %+v, %v", res, err)
	}
	// An orphan object is swept once old enough.
	_, _ = e.disk.WriteBytes(ctx, "orphans/lost.png", []byte(pngData), filesystem.WriteOptions{})
	if res, err := run(t, jobs.SweepOrphans(e.store, "media", "orphans", time.Hour), start.Add(96*time.Hour)); err != nil || res.Count != 1 {
		t.Errorf("orphans: %+v, %v", res, err)
	}
}

// TestSweepLocks: a lock is kept while it holds and swept once expired.
func TestSweepLocks(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	f := e.upload(t, "s1")
	if _, err := e.locks.Lock(ctx, e.store, "s1", f.ID, "alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	j := jobs.SweepLocks(e.locks, e.store)
	if j.Name != jobs.NameSweepLocks {
		t.Errorf("name %q", j.Name)
	}
	if res, err := run(t, j, e.now.Add(time.Minute)); err != nil || res.Count != 0 {
		t.Errorf("live lock: %+v, %v", res, err)
	}
	if res, err := run(t, j, e.now.Add(2*time.Hour)); err != nil || res.Count != 1 {
		t.Errorf("expired lock: %+v, %v", res, err)
	}
	if _, held, err := e.locks.LockOf(ctx, e.store, "s1", f.ID); err != nil || held {
		t.Errorf("after sweep: held %v, %v", held, err)
	}
}

func TestRetentionMustBePositive(t *testing.T) {
	e := newEnv(t)
	for _, j := range []jobs.Job{
		jobs.SweepTrash(e.store, 0),
		jobs.SweepOrphans(e.store, "media", "x", -time.Hour),
		jobs.UploadSweep(e.disk, "x", 0),
		jobs.SweepAbandoned(e.disk, 0),
		jobs.AuditPrune(e.audit, e.store, 0),
		jobs.SharePrune(e.share, e.store, 0),
	} {
		if _, err := run(t, j, e.now); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || !strings.Contains(err.Error(), j.Name) {
			t.Errorf("%s with no retention: %v", j.Name, err)
		}
	}
}

func TestQuotaRecount(t *testing.T) {
	e := newEnv(t)
	e.upload(t, "s1")
	e.upload(t, "s2")
	scopes := func(context.Context) ([]string, error) { return []string{"s1", "s2"}, nil }
	j := jobs.QuotaRecount(e.quota, e.store, scopes)
	if res, err := run(t, j, e.now); err != nil || res.Count != 2 || j.Name != jobs.NameQuotaRecount {
		t.Fatalf("recount: %+v, %v", res, err)
	}
	usage, err := e.quota.Usage(t.Context(), e.store, "s1")
	if err != nil || usage.Files != 1 || usage.Bytes != int64(len(pngData)) {
		t.Errorf("usage %+v, %v", usage, err)
	}
	// The scope list failing fails the run; a failing scope is named and
	// does not stop the others.
	broken := jobs.QuotaRecount(e.quota, e.store, func(context.Context) ([]string, error) { return nil, filesystem.ErrUnavailable })
	if _, err := run(t, broken, e.now); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("scope list failure: %v", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	res, err := j.Run(cancelled, e.now)
	if err == nil || !strings.Contains(err.Error(), `scope "s1"`) || res.Count != 0 {
		t.Errorf("cancelled recount: %+v, %v", res, err)
	}
}

func TestFuncAndNamed(t *testing.T) {
	j := jobs.Func("variants.eager", func(context.Context, time.Time) (jobs.Result, error) { return jobs.Result{Count: 3}, nil })
	other := j.Named("variants.eager.avatars")
	if j.Name != "variants.eager" || other.Name != "variants.eager.avatars" {
		t.Errorf("names %q %q", j.Name, other.Name)
	}
	if res, err := run(t, other, time.Now()); err != nil || res.Count != 3 {
		t.Errorf("run %+v, %v", res, err)
	}
}
