// Package river runs the maintenance jobs of github.com/ubgo/filesystem/jobs
// on River, the Postgres job queue (github.com/riverqueue/river), for
// applications that already run River: one worker kind executes every job
// by name, and each Schedule becomes a River periodic job, so River's
// leader election, retries and uniqueness apply.
//
//	worker, _ := fsriver.NewWorker(fsriver.WorkerOptions{}, jobList...)
//	workers := river.NewWorkers()
//	river.AddWorker(workers, worker)
//	client, _ := river.NewClient(riverpgxv5.New(pool), &river.Config{
//		Queues:       map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 2}},
//		Workers:      workers,
//		PeriodicJobs: fsriver.Periodic(schedules...),
//	})
package river

import (
	"context"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/jobs/diskjobs"
)

// Kind is the River job kind every filesystem maintenance job is
// inserted as; Args.Job says which job.
const Kind = "filesystem_maintenance"

// Args are the River arguments of a maintenance job: its diskjobs.Job name.
type Args struct {
	Job string `json:"job" river:"unique"`
}

// Kind implements river.JobArgs.
func (Args) Kind() string { return Kind }

// inFlight are the states in which a job counts as already queued or
// running: a second insert of the same job is then skipped, which makes
// every job single-flight across all the processes sharing the queue.
var inFlight = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRunning,
	rivertype.JobStateRetryable,
	rivertype.JobStateScheduled,
}

// InsertOpts are the insert options of a maintenance job: unique by job
// name while one is queued or running.
func InsertOpts() *river.InsertOpts { return PeriodicOptions{}.InsertOpts() }

// PeriodicOptions tune the jobs Periodic and EnqueueWith insert. The zero
// value is Periodic's behaviour: River's default queue and attempts.
type PeriodicOptions struct {
	// Queue is the River queue the jobs go to, so maintenance gets its
	// own workers; "" is river.QueueDefault. The client must be
	// configured to work it.
	Queue string
	// MaxAttempts is each job's attempt budget; 0 keeps River's default.
	MaxAttempts int
}

// InsertOpts are the insert options under o: InsertOpts' uniqueness, plus
// o's queue and attempts. Uniqueness is never dropped, so a job stays
// single-flight across the fleet whatever queue it runs on.
func (o PeriodicOptions) InsertOpts() *river.InsertOpts {
	return &river.InsertOpts{Queue: o.Queue, MaxAttempts: o.MaxAttempts, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: inFlight}}
}

// nextFunc adapts Schedule.Next to river.PeriodicSchedule.
type nextFunc func(time.Time) time.Time

// Next implements river.PeriodicSchedule.
func (f nextFunc) Next(t time.Time) time.Time { return f(t) }

// WorkerOptions configure a Worker. The zero value is usable.
type WorkerOptions struct {
	// OnRun receives the report of every run, as diskjobs.SchedulerOptions.OnRun
	// does; nil discards them. River also records the error of a failed
	// run and retries it.
	OnRun func(diskjobs.Report)
	// Now is the clock jobs are given; nil means time.Now.
	Now func() time.Time
	// Timeout bounds one run; 0 keeps the River client's JobTimeout.
	Timeout time.Duration
	// TimeoutFor bounds one run of the named job, so a quick sweep and an
	// hours-long export need not share one bound. Nil, or a zero return,
	// falls back to Timeout; any other value is passed to River as is (a
	// negative one is River's "no timeout").
	TimeoutFor func(job string) time.Duration
}

// Worker executes maintenance jobs by name. Register it with
// river.AddWorker.
type Worker struct {
	river.WorkerDefaults[Args]
	opts WorkerOptions
	jobs map[string]diskjobs.Job
}

// NewWorker returns a Worker for list. A job without a name or a Run
// function, or two jobs with one name, are CodeInvalidArgument.
func NewWorker(opts WorkerOptions, list ...diskjobs.Job) (*Worker, error) {
	if opts.OnRun == nil {
		opts.OnRun = func(diskjobs.Report) {}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	w := &Worker{opts: opts, jobs: make(map[string]diskjobs.Job, len(list))}
	for _, j := range list {
		if j.Name == "" || j.Run == nil {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "a River maintenance job needs a name and a Run function")
		}
		if _, dup := w.jobs[j.Name]; dup {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "job %s is registered twice", j.Name)
		}
		w.jobs[j.Name] = j
	}
	return w, nil
}

// Work implements river.Worker: it runs the named job once. An unknown
// name cancels the River job rather than retrying it, since no retry can
// make it known; a job's own error is returned, so River retries it.
func (w *Worker) Work(ctx context.Context, job *river.Job[Args]) error {
	j, ok := w.jobs[job.Args.Job]
	if !ok {
		return river.JobCancel(filesystem.Errorf(filesystem.CodeNotFound, "no maintenance job %s is registered", job.Args.Job))
	}
	started := w.opts.Now()
	res, err := j.Run(ctx, started)
	w.opts.OnRun(diskjobs.Report{Job: j.Name, Started: started, Duration: w.opts.Now().Sub(started), Result: res, Err: err})
	return err
}

// periodicSchedule is when River inserts sc's job: at Next's times when
// it is set, else every Every.
func periodicSchedule(sc diskjobs.Schedule) river.PeriodicSchedule {
	if sc.Next != nil {
		return nextFunc(sc.Next)
	}
	return river.PeriodicInterval(sc.Every)
}

// Timeout implements river.Worker: WorkerOptions.TimeoutFor for the job
// when it gives one, else WorkerOptions.Timeout, or 0 for the client's
// default. A nil job (no name to ask about) gets WorkerOptions.Timeout.
func (w *Worker) Timeout(job *river.Job[Args]) time.Duration {
	if w.opts.TimeoutFor != nil && job != nil {
		if d := w.opts.TimeoutFor(job.Args.Job); d != 0 {
			return d
		}
	}
	return w.opts.Timeout
}

// Periodic turns schedules into River periodic jobs: each inserts its job
// every Schedule.Every, or at the times Schedule.Next names (River's
// leader runs the schedule, so one instance inserts for the whole fleet),
// and once at start when RunAtStart is set. Jitter is not used: with a
// single leader inserting, fleet-wide spreading is unnecessary. Each
// periodic job's ID is the job's name. It is PeriodicWith with the zero
// options.
func Periodic(schedules ...diskjobs.Schedule) []*river.PeriodicJob {
	return PeriodicWith(PeriodicOptions{}, schedules...)
}

// PeriodicWith is Periodic inserting onto o's queue with o's attempts.
func PeriodicWith(o PeriodicOptions, schedules ...diskjobs.Schedule) []*river.PeriodicJob {
	out := make([]*river.PeriodicJob, 0, len(schedules))
	for _, sc := range schedules {
		name := sc.Job.Name
		out = append(out, river.NewPeriodicJob(
			periodicSchedule(sc),
			func() (river.JobArgs, *river.InsertOpts) { return Args{Job: name}, o.InsertOpts() },
			&river.PeriodicJobOpts{ID: name, RunOnStart: sc.RunAtStart},
		))
	}
	return out
}

// Enqueue inserts one run of the named job now, for an admin button; a
// run already queued or running makes it a no-op (InsertOpts).
func Enqueue[TTx any](ctx context.Context, client *river.Client[TTx], name string) error {
	return EnqueueWith(ctx, client, name, PeriodicOptions{})
}

// EnqueueWith is Enqueue onto o's queue with o's attempts, so an admin
// run lands where the periodic ones do.
func EnqueueWith[TTx any](ctx context.Context, client *river.Client[TTx], name string, o PeriodicOptions) error {
	_, err := client.Insert(ctx, Args{Job: name}, o.InsertOpts())
	return err
}
