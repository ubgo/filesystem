package diskjobs

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/ubgo/filesystem"
)

// Schedule runs Job every Every, or at the times Next names, each wait
// lengthened by a random duration up to Jitter, so many instances of an
// application do not all sweep at the same moment.
type Schedule struct {
	Job Job
	// Every is the interval between runs. Exactly one of Every and Next
	// is set.
	Every time.Duration
	// Next, when set, replaces Every with a calendar: given the current
	// time it returns the next time to run, which must be after it. Daily
	// gives "every day at 03:00"; a cron library's schedule plugs in by
	// its method value (robfig/cron's Schedule.Next has this signature).
	// A Next that returns a time not after the one it was given stops the
	// schedule with a CodeInvalidArgument report instead of running in a
	// tight loop.
	Next   func(now time.Time) time.Time
	Jitter time.Duration
	// RunAtStart runs the job once as soon as the scheduler starts,
	// before the first wait.
	RunAtStart bool
	// DryRun runs Job.DryRun instead of Job.Run, so a new or changed
	// sweep can be scheduled and watched (OnRun reports what it would
	// take) before it is allowed to delete anything. A job without a
	// DryRun is refused with CodeUnsupported, never run for real.
	DryRun bool
}

// job is the job the schedule runs: its dry run when DryRun is set.
func (sc Schedule) job() Job {
	j := sc.Job
	if sc.DryRun {
		j.Run = j.DryRun
	}
	return j
}

// Report is one finished (or skipped) run, given to OnRun.
type Report struct {
	Job string
	// DryRun is true when the run was a dry run: Result says what a real
	// run would have done, and nothing changed.
	DryRun   bool
	Started  time.Time
	Duration time.Duration
	Result   Result
	// Err is the run's error (a panic in the job becomes one, with code
	// unknown), ErrRunning when the run was skipped because the previous
	// one had not finished, or nil.
	Err error
}

// ErrRunning reports a run skipped because the same job was still running
// in this scheduler. Jobs are single-flight per scheduler: two runs of one
// job never overlap here (across processes, use a runner with uniqueness,
// such as River's or Asynq's).
var ErrRunning error = filesystem.Errorf(filesystem.CodeUnavailable, "the job is already running")

// SchedulerOptions configure a Scheduler. The zero value is usable.
type SchedulerOptions struct {
	// OnRun receives every report, errors included; nil discards them. It
	// is the place to log and to count failures. It is called from the
	// job's goroutine and must not block for long.
	OnRun func(Report)
	// Now is the clock jobs are given; nil means time.Now.
	Now func() time.Time
	// After waits like time.After; nil means a real timer. It is the
	// escape hatch for tests that must not wait.
	After func(d time.Duration) <-chan time.Time
}

// Scheduler runs Schedules in this process. It has no state outside the
// value: nothing is persisted, and a restart starts every wait afresh,
// which suits maintenance jobs that finish what earlier runs left.
type Scheduler struct {
	opts    SchedulerOptions
	mu      sync.Mutex
	running map[string]bool
	jobs    map[string]Schedule
}

// NewScheduler returns a Scheduler with opts.
func NewScheduler(opts SchedulerOptions) *Scheduler {
	if opts.OnRun == nil {
		opts.OnRun = func(Report) {}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.After == nil {
		opts.After = time.After
	}
	return &Scheduler{opts: opts, running: map[string]bool{}, jobs: map[string]Schedule{}}
}

// Run runs every schedule until ctx ends, then waits for runs in progress
// to return (they see ctx cancelled) and returns nil. It refuses, before
// starting anything, schedules with an empty or repeated job name, no Run
// function, a non-positive Every or a negative Jitter
// (CodeInvalidArgument), and a dry-run schedule of a job without a
// DryRun (CodeUnsupported).
func (s *Scheduler) Run(ctx context.Context, schedules ...Schedule) error {
	if err := s.register(schedules); err != nil {
		return err
	}
	var wg sync.WaitGroup
	for _, sc := range schedules {
		wg.Go(func() { s.loop(ctx, sc) })
	}
	wg.Wait()
	return nil
}

// register validates schedules and records their jobs for RunNow.
func (s *Scheduler) register(schedules []Schedule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, sc := range schedules {
		switch {
		case sc.Job.Name == "" || sc.Job.Run == nil:
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "a scheduled job needs a name and a Run function")
		case sc.DryRun && sc.Job.DryRun == nil:
			return filesystem.Errorf(filesystem.CodeUnsupported, "job %s has no dry run: it cannot list what it would act on without acting (for SweepAbandoned, the disk lacks the FindAbandoned capability)", sc.Job.Name)
		case seen[sc.Job.Name] || s.jobs[sc.Job.Name].Job.Name != "":
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "job %s is scheduled twice", sc.Job.Name)
		case sc.Next != nil && sc.Every != 0:
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "job %s: set Every or Next, not both", sc.Job.Name)
		case (sc.Next == nil && sc.Every <= 0) || sc.Jitter < 0:
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "job %s: interval %s must be positive and jitter %s not negative", sc.Job.Name, sc.Every, sc.Jitter)
		}
		seen[sc.Job.Name] = true
	}
	for _, sc := range schedules {
		s.jobs[sc.Job.Name] = sc
	}
	return nil
}

// loop waits and runs one schedule until ctx ends.
func (s *Scheduler) loop(ctx context.Context, sc Schedule) {
	if sc.RunAtStart {
		s.runOnce(ctx, sc)
	}
	for {
		wait, err := s.wait(sc)
		if err != nil {
			s.opts.OnRun(Report{Job: sc.Job.Name, DryRun: sc.DryRun, Started: s.opts.Now(), Err: err})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-s.opts.After(wait + jitter(sc.Jitter)):
		}
		if ctx.Err() != nil {
			return
		}
		s.runOnce(ctx, sc)
	}
}

// wait is how long sc waits before its next run, without jitter: Every,
// or the time from now until Next(now). A Next that does not move
// forward is an error, which ends the schedule.
func (s *Scheduler) wait(sc Schedule) (time.Duration, error) {
	if sc.Next == nil {
		return sc.Every, nil
	}
	now := s.opts.Now()
	next := sc.Next(now)
	if !next.After(now) {
		return 0, filesystem.Errorf(filesystem.CodeInvalidArgument, "job %s: Next(%s) returned %s, which is not later; the schedule stopped", sc.Job.Name, now.Format(time.RFC3339), next.Format(time.RFC3339))
	}
	return next.Sub(now), nil
}

// Daily returns a Schedule.Next that runs every day at hour:minute in
// loc (time.UTC when nil): "sweep at 03:00". On a day when that wall time
// does not exist or repeats (a daylight-saving change), it runs once, at
// the time time.Date normalises it to.
func Daily(hour, minute int, loc *time.Location) func(now time.Time) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	return func(now time.Time) time.Time {
		y, m, d := now.In(loc).Date()
		at := time.Date(y, m, d, hour, minute, 0, 0, loc)
		if !at.After(now) {
			at = time.Date(y, m, d+1, hour, minute, 0, 0, loc)
		}
		return at
	}
}

// jitter is a random duration in [0, max]. It needs no cryptographic
// randomness.
func jitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(max) + 1))
}

// RunNow runs a job registered by Run immediately, in the caller's
// goroutine, for an admin button or a test. It returns the report, which
// OnRun also receives; a job still running gives ErrRunning, and an
// unknown name CodeNotFound.
func (s *Scheduler) RunNow(ctx context.Context, name string) Report {
	s.mu.Lock()
	sc, ok := s.jobs[name]
	s.mu.Unlock()
	if !ok {
		return Report{Job: name, Err: filesystem.Errorf(filesystem.CodeNotFound, "no job %s is scheduled", name)}
	}
	return s.runOnce(ctx, sc)
}

// runOnce runs the schedule's job (its dry run when the schedule says so)
// unless it is already running, and reports.
func (s *Scheduler) runOnce(ctx context.Context, sc Schedule) Report {
	job := sc.job()
	started := s.opts.Now()
	s.mu.Lock()
	busy := s.running[job.Name]
	s.running[job.Name] = true
	s.mu.Unlock()
	if busy {
		r := Report{Job: job.Name, DryRun: sc.DryRun, Started: started, Err: ErrRunning}
		s.opts.OnRun(r)
		return r
	}
	defer func() {
		s.mu.Lock()
		delete(s.running, job.Name)
		s.mu.Unlock()
	}()
	res, err := safeRun(ctx, job, started)
	r := Report{Job: job.Name, DryRun: sc.DryRun, Started: started, Duration: s.opts.Now().Sub(started), Result: res, Err: err}
	s.opts.OnRun(r)
	return r
}

// safeRun runs job, turning a panic into an error, so one faulty job
// cannot bring down the process that schedules the others.
func safeRun(ctx context.Context, job Job, now time.Time) (res Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = filesystem.Errorf(filesystem.CodeUnknown, "job %s panicked: %v", job.Name, p)
		}
	}()
	return job.Run(ctx, now)
}
