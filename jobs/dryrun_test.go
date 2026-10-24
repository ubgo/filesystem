package jobs_test

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/jobs"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
)

// snapshot is everything a sweep could change: every object on the disk
// (key, size, modification time) and every file row of the scope, live
// and trashed.
type snapshot struct {
	objects []filesystem.Entry
	rows    []records.File
}

func (e *env) snapshot(t *testing.T, scope string) snapshot {
	t.Helper()
	var s snapshot
	for en, err := range e.disk.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		s.objects = append(s.objects, filesystem.Entry{Key: en.Key, Size: en.Size, ModTime: en.ModTime})
	}
	for _, trashed := range []bool{false, true} {
		page, err := e.store.Files(t.Context(), records.FileQuery{Scope: scope, Trashed: trashed, Limit: records.MaxPageSize})
		if err != nil {
			t.Fatal(err)
		}
		s.rows = append(s.rows, page.Files...)
	}
	return s
}

// TestDryRunsChangeNothing schedules every sweep that can preview itself
// as a dry run over a disk and a store holding something for each to take,
// checks each reports exactly what its real run then removes, and that
// the dry runs changed nothing at all.
func TestDryRunsChangeNothing(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	trashed := e.upload(t, "acme")
	kept := e.upload(t, "acme")
	if err := e.store.Trash(ctx, "acme", trashed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.disk.WriteBytes(ctx, "orphans/lost.bin", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.disk.WriteBytes(ctx, "incoming/stale.bin", []byte("y"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	uploadID, err := e.disk.CreateMultipart(ctx, "big.bin", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(48 * time.Hour)

	trash := jobs.SweepTrash(e.store, time.Hour)
	orphans := jobs.SweepOrphans(e.store, "media", "orphans", time.Hour)
	uploads := jobs.UploadSweep(e.disk, "incoming", time.Hour)
	abandoned := jobs.SweepAbandoned(e.disk, time.Hour)
	want := map[string][]string{
		trash.Name:     {trashed.ID},
		orphans.Name:   {"orphans/lost.bin"},
		uploads.Name:   {"incoming/stale.bin"},
		abandoned.Name: {"multipart:" + uploadID + " big.bin"},
	}
	all := []jobs.Job{trash, orphans, uploads, abandoned}

	before := e.snapshot(t, "acme")
	reports := map[string]jobs.Report{}
	s := jobs.NewScheduler(jobs.SchedulerOptions{Now: func() time.Time { return e.now }, OnRun: func(r jobs.Report) { reports[r.Job] = r }})
	runCtx, cancel := context.WithCancel(ctx)
	cancel() // register only; RunNow below drives each run
	var schedules []jobs.Schedule
	for _, j := range all {
		schedules = append(schedules, jobs.Schedule{Job: j, Every: time.Hour, DryRun: true})
	}
	if err := s.Run(runCtx, schedules...); err != nil {
		t.Fatal(err)
	}
	for _, j := range all {
		r := s.RunNow(ctx, j.Name)
		if r.Err != nil || !r.DryRun {
			t.Fatalf("%s: dry run report %+v", j.Name, r)
		}
		if !slices.Equal(r.Result.Planned, want[j.Name]) || r.Result.Count != int64(len(want[j.Name])) {
			t.Fatalf("%s planned %v (count %d), want %v", j.Name, r.Result.Planned, r.Result.Count, want[j.Name])
		}
	}
	if after := e.snapshot(t, "acme"); !reflect.DeepEqual(before, after) {
		t.Fatalf("a dry run changed state:\nbefore %+v\nafter  %+v", before, after)
	}
	if _, err := e.disk.ListParts(ctx, "big.bin", uploadID); err != nil {
		t.Fatalf("the abandoned-upload dry run aborted the upload: %v", err)
	}

	// The real runs take exactly what the dry runs planned.
	for _, j := range all {
		res, err := run(t, j, e.now)
		if err != nil || res.Count != int64(len(want[j.Name])) {
			t.Fatalf("%s: real run %+v, %v", j.Name, res, err)
		}
	}
	if _, err := e.store.File(ctx, "acme", trashed.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("trashed file after the sweep: %v", err)
	}
	if _, err := e.store.File(ctx, "acme", kept.ID); err != nil {
		t.Fatalf("live file swept: %v", err)
	}
}

// noPreview is the memory driver able to sweep but not to preview.
type noPreview struct{ *memory.Driver }

func (n noPreview) Capabilities() filesystem.Capabilities {
	c := n.Driver.Capabilities()
	c.FindAbandoned = false
	return c
}

// TestDryRunScheduleRefusedWithoutPreview: a job that cannot preview
// itself (SweepAbandoned on a disk without FindAbandoned) is refused as a
// dry-run schedule with CodeUnsupported naming the reason, rather than
// run for real.
func TestDryRunScheduleRefusedWithoutPreview(t *testing.T) {
	disk, err := filesystem.NewDisk("sweeponly", noPreview{memory.New()})
	if err != nil {
		t.Fatal(err)
	}
	if jobs.SweepAbandoned(disk, time.Hour).DryRun != nil {
		t.Fatal("a dry run offered on a disk that cannot preview")
	}
	s := jobs.NewScheduler(jobs.SchedulerOptions{})
	// A cancelled context: were the schedule accepted, Run would return
	// nil at once instead of waiting.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = s.Run(ctx, jobs.Schedule{Job: jobs.SweepAbandoned(disk, time.Hour), Every: time.Hour, DryRun: true})
	if filesystem.CodeOf(err) != filesystem.CodeUnsupported || !strings.Contains(err.Error(), "has no dry run") {
		t.Fatalf("got %v, want CodeUnsupported naming the reason", err)
	}
}

// TestRetentionDryRun: the retention dry runs refuse a non-positive
// retention as the real runs do, and SweepRetention's dry run reports
// nothing without rules.
func TestRetentionDryRun(t *testing.T) {
	e := newEnv(t)
	if _, err := jobs.SweepTrash(e.store, 0).DryRun(t.Context(), e.now); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("zero retention dry run: %v", err)
	}
	res, err := jobs.SweepRetention(e.store).DryRun(t.Context(), e.now)
	if err != nil || res.Count != 0 || res.Planned != nil {
		t.Fatalf("retention dry run without rules: %+v, %v", res, err)
	}
}

// TestAbandonedItem pins the dry-run naming of a leftover.
func TestAbandonedItem(t *testing.T) {
	for _, c := range []struct {
		a    filesystem.Abandoned
		want string
	}{
		{filesystem.Abandoned{Kind: filesystem.AbandonedMultipart, ID: "7", Key: "videos/big file.mp4"}, "multipart:7 videos/big file.mp4"},
		{filesystem.Abandoned{Kind: filesystem.AbandonedStaging, ID: "ABC"}, "staging:ABC"},
	} {
		if got := jobs.AbandonedItem(c.a); got != c.want {
			t.Errorf("AbandonedItem(%+v) = %q, want %q", c.a, got, c.want)
		}
	}
}

// TestSweepAbandonedDryRunErrors: the preview refuses a non-positive age
// like the sweep, and reports a failing disk.
func TestSweepAbandonedDryRunErrors(t *testing.T) {
	e := newEnv(t)
	if _, err := jobs.SweepAbandoned(e.disk, 0).DryRun(t.Context(), e.now); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("zero age dry run: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := jobs.SweepAbandoned(e.disk, time.Hour).DryRun(ctx, e.now); err == nil {
		t.Fatal("a dry run on a cancelled context succeeded")
	}
}
