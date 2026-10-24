package asynq_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/ubgo/filesystem"
	fsasynq "github.com/ubgo/filesystem/jobs/asynq"
	"github.com/ubgo/filesystem/jobs/diskjobs"
)

// TestRegisterWith pins the options seam without Redis (the scheduler
// contacts it only when started): a calendar schedule needs a Spec and is
// refused without one, a Spec's cronspec is what gets registered, and a
// Spec error fails closed.
func TestRegisterWith(t *testing.T) {
	sched := asynq.NewScheduler(asynq.RedisClientOpt{Addr: "127.0.0.1:1"}, nil)
	ok := diskjobs.Func("a", func(context.Context, time.Time) (diskjobs.Result, error) { return diskjobs.Result{}, nil })
	nightly := diskjobs.Schedule{Job: ok, Next: diskjobs.Daily(3, 0, nil)}
	if err := fsasynq.RegisterWith(t.Context(), sched, nil, fsasynq.RegisterOptions{}, nightly); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("Next without Spec: %v", err)
	}
	var asked diskjobs.Schedule
	spec := func(sc diskjobs.Schedule) (string, error) { asked = sc; return "0 3 * * *", nil }
	opts := fsasynq.RegisterOptions{Queue: "maintenance", MaxRetry: -1, Timeout: time.Minute, Spec: spec}
	if err := fsasynq.RegisterWith(t.Context(), sched, nil, opts, nightly); err != nil || asked.Job.Name != "a" {
		t.Errorf("Spec: %v, asked about %q", err, asked.Job.Name)
	}
	bad := errors.New("no spec")
	opts.Spec = func(diskjobs.Schedule) (string, error) { return "", bad }
	if err := fsasynq.RegisterWith(t.Context(), sched, nil, opts, nightly); !errors.Is(err, bad) {
		t.Errorf("a Spec error: %v", err)
	}
	// A cronspec Asynq cannot parse is refused by Asynq.
	opts.Spec = func(diskjobs.Schedule) (string, error) { return "not a spec", nil }
	if err := fsasynq.RegisterWith(t.Context(), sched, nil, opts, nightly); err == nil {
		t.Error("an invalid cronspec was registered")
	}
}

// TestRegisterWithUnique pins that a refused uniqueness window fails both
// RegisterWith and EnqueueWith before anything reaches Asynq (R-C9).
func TestRegisterWithUnique(t *testing.T) {
	sched := asynq.NewScheduler(asynq.RedisClientOpt{Addr: "127.0.0.1:1"}, nil)
	ok := diskjobs.Func("a", func(context.Context, time.Time) (diskjobs.Result, error) { return diskjobs.Result{}, nil })
	every := diskjobs.Schedule{Job: ok, Every: time.Minute}
	off := fsasynq.RegisterOptions{Unique: func(string) time.Duration { return -1 }}
	if err := fsasynq.RegisterWith(t.Context(), sched, nil, off, every); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("RegisterWith with a negative window: %v", err)
	}
	if err := fsasynq.EnqueueWith(t.Context(), nil, "a", off); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("EnqueueWith with a negative window: %v", err)
	}
	custom := fsasynq.RegisterOptions{Unique: func(string) time.Duration { return 30 * time.Second }}
	if err := fsasynq.RegisterWith(t.Context(), sched, nil, custom, every); err != nil {
		t.Errorf("RegisterWith with a 30s window: %v", err)
	}
}
