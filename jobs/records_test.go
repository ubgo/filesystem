package jobs_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/jobs"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// TestRecordsLifecycleJobs: SweepExpired purges a file once its expiry is
// past, SweepRetention detaches and purges what its rule says, each under
// its own name, and a second run finds nothing.
func TestRecordsLifecycleJobs(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	db, dialect := recordstest.Open(t)
	store, err := records.New(db, dialect, mgr, records.WithClock(func() time.Time { return now }), records.WithOwnerTypes("chat"),
		records.WithRetention(records.Retention{OwnerType: "chat", MaxAge: time.Hour}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upload(ctx, "s", "media", strings.NewReader(pngData), "story.png", images, records.Expires(now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	msg, err := store.Upload(ctx, "s", "media", strings.NewReader(pngData), "photo.png", images)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Attach(ctx, "s", msg.ID, records.Owner{Type: "chat", ID: "c1"}, "photo"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		job  jobs.Job
		name string
	}{{jobs.SweepExpired(store), jobs.NameSweepExpired}, {jobs.SweepRetention(store), jobs.NameSweepRetention}} {
		if c.job.Name != c.name {
			t.Errorf("name %q, want %q", c.job.Name, c.name)
		}
		if res, err := run(t, c.job, now.Add(time.Minute)); err != nil || res.Count != 0 {
			t.Errorf("%s early: %+v, %v", c.name, res, err)
		}
		if res, err := run(t, c.job, now.Add(2*time.Hour)); err != nil || res.Count != 1 {
			t.Errorf("%s: %+v, %v; want 1", c.name, res, err)
		}
		if res, err := run(t, c.job, now.Add(2*time.Hour)); err != nil || res.Count != 0 {
			t.Errorf("%s again: %+v, %v", c.name, res, err)
		}
	}
	page, err := store.Files(ctx, records.FileQuery{Scope: "s"})
	if err != nil || len(page.Files) != 0 {
		t.Fatalf("files left after both sweeps: %+v, %v", page.Files, err)
	}
}
