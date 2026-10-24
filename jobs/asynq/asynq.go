// Package asynq runs the maintenance jobs of github.com/ubgo/filesystem/jobs
// on Asynq, the Redis task queue (github.com/hibiken/asynq), for
// applications that already run Asynq: one task type executes every job by
// name, each Schedule becomes an Asynq scheduler entry, and every run is
// unique while it is queued, so two servers never run one job at once.
//
//	handler, _ := fsasynq.NewHandler(fsasynq.HandlerOptions{}, jobList...)
//	mux := asynq.NewServeMux()
//	mux.Handle(fsasynq.TaskType, handler)
//	scheduler := asynq.NewScheduler(redisOpt, nil)
//	_ = fsasynq.Register(ctx, scheduler, client, schedules...)
package asynq

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hibiken/asynq"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/jobs/diskjobs"
)

// TaskType is the Asynq task type of every maintenance job; the payload is
// the job's name.
const TaskType = "filesystem:maintenance"

// NewTask is the Asynq task that runs the named job.
func NewTask(name string) *asynq.Task { return asynq.NewTask(TaskType, []byte(name)) }

// HandlerOptions configure a Handler. The zero value is usable.
type HandlerOptions struct {
	// OnRun receives the report of every run, as diskjobs.SchedulerOptions.OnRun
	// does; nil discards them. Asynq also records a failed run's error and
	// retries it.
	OnRun func(diskjobs.Report)
	// Now is the clock jobs are given; nil means time.Now.
	Now func() time.Time
}

// Handler executes maintenance tasks by job name. Register it on an
// asynq.ServeMux under TaskType.
type Handler struct {
	opts HandlerOptions
	jobs map[string]diskjobs.Job
}

// NewHandler returns a Handler for list. A job without a name or a Run
// function, or two jobs with one name, are CodeInvalidArgument.
func NewHandler(opts HandlerOptions, list ...diskjobs.Job) (*Handler, error) {
	if opts.OnRun == nil {
		opts.OnRun = func(diskjobs.Report) {}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	h := &Handler{opts: opts, jobs: make(map[string]diskjobs.Job, len(list))}
	for _, j := range list {
		if j.Name == "" || j.Run == nil {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "an Asynq maintenance job needs a name and a Run function")
		}
		if _, dup := h.jobs[j.Name]; dup {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "job %s is registered twice", j.Name)
		}
		h.jobs[j.Name] = j
	}
	return h, nil
}

// ProcessTask implements asynq.Handler: it runs the job the payload names.
// An unknown name is not retried (asynq.SkipRetry), since no retry can
// make it known; a job's own error is returned, so Asynq retries it.
func (h *Handler) ProcessTask(ctx context.Context, t *asynq.Task) error {
	name := string(t.Payload())
	j, ok := h.jobs[name]
	if !ok {
		return fmt.Errorf("%w: %w", filesystem.Errorf(filesystem.CodeNotFound, "no maintenance job %s is registered", name), asynq.SkipRetry)
	}
	started := h.opts.Now()
	res, err := j.Run(ctx, started)
	h.opts.OnRun(diskjobs.Report{Job: name, Started: started, Duration: h.opts.Now().Sub(started), Result: res, Err: err})
	return err
}

// uniqueFor is how long an enqueued run of sc blocks another of the same
// job when RegisterOptions.Unique does not say: its interval, so a run that
// has not started within one interval is not joined by a second. A
// calendar schedule (Next, with a Spec) has no fixed interval and uses
// defaultUnique.
func uniqueFor(sc diskjobs.Schedule) time.Duration {
	if sc.Every > 0 {
		return sc.Every
	}
	return defaultUnique
}

// minUnique is the shortest uniqueness window Asynq accepts
// (asynq.Unique refuses less than one second when the task is enqueued).
const minUnique = time.Second

// RegisterOptions tune the tasks Register enqueues. The zero value is
// Register's behaviour: Asynq's default queue, retry budget and timeout,
// and an "@every <Every>" entry per schedule.
type RegisterOptions struct {
	// Queue is the Asynq queue the tasks go to, so maintenance gets its
	// own workers and priority; "" is Asynq's default queue. The
	// server must be configured to process it.
	Queue string
	// MaxRetry is each task's retry budget: 0 keeps Asynq's default, a
	// negative value means no retries.
	MaxRetry int
	// Timeout bounds one run; 0 keeps Asynq's default.
	Timeout time.Duration
	// Spec returns the Asynq cronspec of a schedule ("0 3 * * *",
	// "@every 1h"). Nil means "@every <Every>". A schedule with Next set
	// needs it: Asynq's scheduler takes a cronspec, not a function, so
	// Register refuses such a schedule with CodeUnsupported without one.
	// An error fails the registration.
	Spec func(sc diskjobs.Schedule) (string, error)
	// Unique returns how long one queued run of the named job blocks a
	// second run of it, for both scheduled and Enqueue'd tasks: a
	// sub-hourly calendar schedule needs less than the hour Enqueue and
	// cronspec schedules get by default, and a job that runs longer than
	// its interval needs more. Nil, or a zero return, keeps the default
	// (the schedule's interval, else one hour).
	//
	// Invariant: uniqueness itself is not optional, since it is what keeps
	// a fleet of schedulers from running one job twice at once, so a
	// window under one second (Asynq's minimum), negative included, is
	// refused with CodeInvalidArgument rather than turned off.
	Unique func(job string) time.Duration
}

// unique is the Asynq uniqueness option for job under o, fallback being
// the window used when o.Unique is nil or returns zero.
func (o RegisterOptions) unique(job string, fallback time.Duration) (asynq.Option, error) {
	d := fallback
	if o.Unique != nil {
		if custom := o.Unique(job); custom != 0 {
			d = custom
		}
	}
	if d < minUnique {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "job %s: uniqueness window %s is under Asynq's minimum of %s", job, d, minUnique)
	}
	return asynq.Unique(d), nil
}

// options are the Asynq task options o adds, on top of uniqueness.
func (o RegisterOptions) options() []asynq.Option {
	var opts []asynq.Option
	if o.Queue != "" {
		opts = append(opts, asynq.Queue(o.Queue))
	}
	switch {
	case o.MaxRetry > 0:
		opts = append(opts, asynq.MaxRetry(o.MaxRetry))
	case o.MaxRetry < 0:
		opts = append(opts, asynq.MaxRetry(0))
	}
	if o.Timeout > 0 {
		opts = append(opts, asynq.Timeout(o.Timeout))
	}
	return opts
}

// spec is sc's cronspec under o.
func (o RegisterOptions) spec(sc diskjobs.Schedule) (string, error) {
	if o.Spec != nil {
		return o.Spec(sc)
	}
	if sc.Next != nil {
		return "", filesystem.Errorf(filesystem.CodeUnsupported, "job %s: a Next schedule needs RegisterOptions.Spec, since Asynq schedules by cronspec", sc.Job.Name)
	}
	if sc.Every <= 0 {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "job %s: interval %s must be positive", sc.Job.Name, sc.Every)
	}
	return "@every " + sc.Every.String(), nil
}

// Register adds every schedule to scheduler as an "@every" entry whose
// task is unique for one interval, and enqueues the RunAtStart ones now
// through client. Jitter is not used: Asynq's uniqueness already keeps a
// fleet of schedulers from running one job twice. A non-positive interval
// is CodeInvalidArgument. It is RegisterWith with the zero options.
func Register(ctx context.Context, scheduler *asynq.Scheduler, client *asynq.Client, schedules ...diskjobs.Schedule) error {
	return RegisterWith(ctx, scheduler, client, RegisterOptions{}, schedules...)
}

// RegisterWith is Register with options: a dedicated queue, a retry
// budget and timeout, a cronspec for calendar schedules, and a uniqueness
// window per job.
func RegisterWith(ctx context.Context, scheduler *asynq.Scheduler, client *asynq.Client, o RegisterOptions, schedules ...diskjobs.Schedule) error {
	for _, sc := range schedules {
		spec, err := o.spec(sc)
		if err != nil {
			return err
		}
		uniq, err := o.unique(sc.Job.Name, uniqueFor(sc))
		if err != nil {
			return err
		}
		opts := append(o.options(), uniq)
		if _, err := scheduler.Register(spec, NewTask(sc.Job.Name), opts...); err != nil {
			return err
		}
		if sc.RunAtStart {
			if err := enqueue(ctx, client, sc.Job.Name, opts...); err != nil {
				return err
			}
		}
	}
	return nil
}

// defaultUnique is how long Enqueue blocks a second run of one job.
const defaultUnique = time.Hour

// Enqueue queues one run of the named job now, for an admin button. A run
// of it already queued within the last hour makes this a no-op
// (EnqueueWith with RegisterOptions.Unique changes the window).
func Enqueue(ctx context.Context, client *asynq.Client, name string) error {
	return EnqueueWith(ctx, client, name, RegisterOptions{})
}

// EnqueueWith is Enqueue onto o's queue with o's retry budget, timeout
// and uniqueness window, so an admin run lands where the scheduled ones do.
func EnqueueWith(ctx context.Context, client *asynq.Client, name string, o RegisterOptions) error {
	uniq, err := o.unique(name, defaultUnique)
	if err != nil {
		return err
	}
	return enqueue(ctx, client, name, append(o.options(), uniq)...)
}

func enqueue(ctx context.Context, client *asynq.Client, name string, opts ...asynq.Option) error {
	_, err := client.EnqueueContext(ctx, NewTask(name), opts...)
	if errors.Is(err, asynq.ErrDuplicateTask) {
		return nil
	}
	return err
}
