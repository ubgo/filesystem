package jobs_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/jobs"
)

// fakeTimer hands the scheduler channels the test fires, and records the
// waits it asked for.
type fakeTimer struct {
	mu    sync.Mutex
	waits []time.Duration
	fire  chan time.Time
}

func newFakeTimer() *fakeTimer { return &fakeTimer{fire: make(chan time.Time)} }

func (f *fakeTimer) after(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	f.waits = append(f.waits, d)
	f.mu.Unlock()
	return f.fire
}

func counting(name string, count *int64, mu *sync.Mutex) jobs.Job {
	return jobs.Func(name, func(context.Context, time.Time) (jobs.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		*count++
		return jobs.Result{Count: *count}, nil
	})
}

func TestSchedulerRunsOnTheTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	timer := newFakeTimer()
	reports := make(chan jobs.Report, 10)
	fixed := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	s := jobs.NewScheduler(jobs.SchedulerOptions{OnRun: func(r jobs.Report) { reports <- r }, Now: func() time.Time { return fixed }, After: timer.after})
	var mu sync.Mutex
	var n int64
	var seen time.Time
	job := jobs.Func("tick", func(_ context.Context, now time.Time) (jobs.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		seen = now
		return jobs.Result{Count: n}, nil
	})
	done := make(chan error)
	go func() {
		done <- s.Run(ctx, jobs.Schedule{Job: job, Every: time.Hour, Jitter: time.Minute, RunAtStart: true})
	}()
	if r := <-reports; r.Job != "tick" || r.Result.Count != 1 || r.Err != nil || !r.Started.Equal(fixed) {
		t.Errorf("start report %+v", r)
	}
	timer.fire <- fixed
	if r := <-reports; r.Result.Count != 2 {
		t.Errorf("tick report %+v", r)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v", err)
	}
	if !seen.Equal(fixed) {
		t.Errorf("job saw now %v", seen)
	}
	timer.mu.Lock()
	defer timer.mu.Unlock()
	for _, w := range timer.waits {
		if w < time.Hour || w > time.Hour+time.Minute {
			t.Errorf("wait %v outside [Every, Every+Jitter]", w)
		}
	}
}

// endedOnTick is a context whose Done channel never fires but whose Err
// reports cancellation once the timer has fired: the moment a timer and a
// cancellation arrive together, made deterministic.
type endedOnTick struct {
	context.Context
	ended atomic.Bool
}

func (c *endedOnTick) Done() <-chan struct{} { return nil }

func (c *endedOnTick) Err() error {
	if c.ended.Load() {
		return context.Canceled
	}
	return nil
}

func TestSchedulerStopsAfterATickWhenCancelled(t *testing.T) {
	ctx := &endedOnTick{Context: t.Context()}
	var mu sync.Mutex
	var n int64
	s := jobs.NewScheduler(jobs.SchedulerOptions{After: func(time.Duration) <-chan time.Time {
		// The timer fires and the context ends together: the scheduler
		// must not start a run after its context ended.
		ctx.ended.Store(true)
		fire := make(chan time.Time, 1)
		fire <- time.Time{}
		return fire
	}})
	if err := s.Run(ctx, jobs.Schedule{Job: counting("j", &n, &mu), Every: time.Second}); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("ran %d times after the context ended", n)
	}
}

func TestSchedulerValidation(t *testing.T) {
	ok := jobs.Func("a", func(context.Context, time.Time) (jobs.Result, error) { return jobs.Result{}, nil })
	for name, sc := range map[string][]jobs.Schedule{
		"no name":         {{Job: jobs.Job{Run: ok.Run}, Every: time.Second}},
		"no run":          {{Job: jobs.Job{Name: "x"}, Every: time.Second}},
		"twice":           {{Job: ok, Every: time.Second}, {Job: ok, Every: time.Minute}},
		"zero interval":   {{Job: ok}},
		"negative jitter": {{Job: ok, Every: time.Second, Jitter: -1}},
	} {
		err := jobs.NewScheduler(jobs.SchedulerOptions{}).Run(t.Context(), sc...)
		if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A name already registered by an earlier Run is refused too.
	s := jobs.NewScheduler(jobs.SchedulerOptions{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Run(ctx, jobs.Schedule{Job: ok, Every: time.Second}); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(t.Context(), jobs.Schedule{Job: ok, Every: time.Second}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("second registration: %v", err)
	}
}

func TestRunNowIsSingleFlight(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	slow := jobs.Func("slow", func(context.Context, time.Time) (jobs.Result, error) {
		close(started)
		<-release
		return jobs.Result{Count: 7}, nil
	})
	var reports []jobs.Report
	var mu sync.Mutex
	s := jobs.NewScheduler(jobs.SchedulerOptions{OnRun: func(r jobs.Report) { mu.Lock(); reports = append(reports, r); mu.Unlock() }})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_ = s.Run(ctx, jobs.Schedule{Job: slow, Every: time.Hour}) // registers, returns at once
	first := make(chan jobs.Report)
	go func() { first <- s.RunNow(t.Context(), "slow") }()
	<-started
	if r := s.RunNow(t.Context(), "slow"); !errors.Is(r.Err, jobs.ErrRunning) {
		t.Errorf("overlapping run: %+v", r)
	}
	close(release)
	if r := <-first; r.Err != nil || r.Result.Count != 7 {
		t.Errorf("first run %+v", r)
	}
	if r := s.RunNow(t.Context(), "missing"); filesystem.CodeOf(r.Err) != filesystem.CodeNotFound {
		t.Errorf("unknown job: %+v", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 2 {
		t.Errorf("OnRun got %d reports, want the skip and the run", len(reports))
	}
}

func TestPanickingJobIsReported(t *testing.T) {
	s := jobs.NewScheduler(jobs.SchedulerOptions{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_ = s.Run(ctx, jobs.Schedule{Job: jobs.Func("boom", func(context.Context, time.Time) (jobs.Result, error) { panic("bad state") }), Every: time.Hour})
	if r := s.RunNow(t.Context(), "boom"); filesystem.CodeOf(r.Err) != filesystem.CodeUnknown {
		t.Errorf("panic report %+v", r)
	}
	// The scheduler still runs it again: the panic released the job.
	if r := s.RunNow(t.Context(), "boom"); errors.Is(r.Err, jobs.ErrRunning) {
		t.Error("a panicking job stayed marked as running")
	}
}

func TestRealTimerAndJitter(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	ran := make(chan struct{}, 1)
	s := jobs.NewScheduler(jobs.SchedulerOptions{OnRun: func(jobs.Report) {
		select {
		case ran <- struct{}{}:
		default:
		}
	}})
	done := make(chan error)
	go func() {
		done <- s.Run(ctx, jobs.Schedule{Job: jobs.Func("fast", func(context.Context, time.Time) (jobs.Result, error) { return jobs.Result{}, nil }), Every: time.Millisecond, Jitter: time.Millisecond})
	}()
	<-ran
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
