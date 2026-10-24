package diskjobs_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/jobs/diskjobs"
)

// recorder hands the scheduler channels the test fires and records the
// waits it asked for.
type recorder struct {
	mu    sync.Mutex
	waits []time.Duration
	fire  chan time.Time
}

func (r *recorder) after(d time.Duration) <-chan time.Time {
	r.mu.Lock()
	r.waits = append(r.waits, d)
	r.mu.Unlock()
	return r.fire
}

func noop(name string) diskjobs.Job {
	return diskjobs.Func(name, func(context.Context, time.Time) (diskjobs.Result, error) { return diskjobs.Result{}, nil })
}

// TestSchedule_Next pins the calendar seam: the wait is the time until
// Next(now); Every and Next together are refused; and a Next that does
// not move forward stops the schedule with an error report instead of
// spinning.
func TestSchedule_Next(t *testing.T) {
	now := time.Date(2026, 10, 9, 1, 30, 0, 0, time.UTC)
	rec := &recorder{fire: make(chan time.Time)}
	reports := make(chan diskjobs.Report, 4)
	s := diskjobs.NewScheduler(diskjobs.SchedulerOptions{Now: func() time.Time { return now }, After: rec.after, OnRun: func(r diskjobs.Report) { reports <- r }})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() {
		done <- s.Run(ctx, diskjobs.Schedule{Job: noop("nightly"), Next: diskjobs.Daily(3, 0, nil)})
	}()
	rec.fire <- now
	<-reports
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	if len(rec.waits) == 0 || rec.waits[0] != 90*time.Minute {
		t.Errorf("waits %v, want 1h30m until 03:00", rec.waits)
	}
	rec.mu.Unlock()

	both := diskjobs.Schedule{Job: noop("x"), Every: time.Hour, Next: diskjobs.Daily(3, 0, nil)}
	if err := diskjobs.NewScheduler(diskjobs.SchedulerOptions{}).Run(t.Context(), both); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("Every and Next together: %v", err)
	}

	stuck := diskjobs.Schedule{Job: noop("stuck"), Next: func(t time.Time) time.Time { return t }}
	var got []diskjobs.Report
	s = diskjobs.NewScheduler(diskjobs.SchedulerOptions{Now: func() time.Time { return now }, After: rec.after, OnRun: func(r diskjobs.Report) { got = append(got, r) }})
	if err := s.Run(t.Context(), stuck); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || filesystem.CodeOf(got[0].Err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a Next that does not move forward: %+v", got)
	}
}

func TestDaily(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no time zone database")
	}
	for _, c := range []struct {
		now, want time.Time
		loc       *time.Location
	}{
		{time.Date(2026, 10, 9, 1, 0, 0, 0, time.UTC), time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC), nil},
		{time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC), time.Date(2026, 10, 10, 3, 0, 0, 0, time.UTC), nil},
		{time.Date(2026, 10, 9, 23, 0, 0, 0, time.UTC), time.Date(2026, 10, 10, 3, 0, 0, 0, time.UTC), nil},
		{time.Date(2026, 12, 31, 4, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 3, 0, 0, 0, time.UTC), nil},
		// 06:00 UTC is 02:00 in New York: today's 03:00 there is ahead.
		{time.Date(2026, 10, 9, 6, 0, 0, 0, time.UTC), time.Date(2026, 10, 9, 3, 0, 0, 0, ny), ny},
	} {
		if got := diskjobs.Daily(3, 0, c.loc)(c.now); !got.Equal(c.want) {
			t.Errorf("Daily(3:00)(%s) = %s, want %s", c.now, got, c.want)
		}
	}
}
