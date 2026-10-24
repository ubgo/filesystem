package asynq

import (
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/jobs/diskjobs"
)

// TestRegisterOptions_Options pins how each field becomes an Asynq task
// option, and that the zero value adds none (Register's behaviour).
func TestRegisterOptions_Options(t *testing.T) {
	if got := (RegisterOptions{}).options(); len(got) != 0 {
		t.Fatalf("the zero options add %v", got)
	}
	got := map[asynq.OptionType]any{}
	for _, o := range (RegisterOptions{Queue: "maintenance", MaxRetry: -1, Timeout: time.Minute}).options() {
		got[o.Type()] = o.Value()
	}
	if got[asynq.QueueOpt] != "maintenance" || got[asynq.MaxRetryOpt] != 0 || got[asynq.TimeoutOpt] != time.Minute {
		t.Fatalf("options %v", got)
	}
	for _, o := range (RegisterOptions{MaxRetry: 3}).options() {
		if o.Type() == asynq.MaxRetryOpt && o.Value() != 3 {
			t.Fatalf("MaxRetry 3 gave %v", o.Value())
		}
	}
}

// TestRegisterOptions_Unique pins the uniqueness seam (R-C9): the default
// window is the fallback (a schedule's interval, else an hour), a custom
// window is used per job, a zero return keeps the default, and a window
// under Asynq's minimum fails closed instead of turning uniqueness off.
func TestRegisterOptions_Unique(t *testing.T) {
	window := func(o RegisterOptions, job string, fallback time.Duration) time.Duration {
		t.Helper()
		opt, err := o.unique(job, fallback)
		if err != nil {
			t.Fatalf("unique(%s): %v", job, err)
		}
		if opt.Type() != asynq.UniqueOpt {
			t.Fatalf("unique(%s) gave option %v", job, opt.Type())
		}
		d, ok := opt.Value().(time.Duration)
		if !ok {
			t.Fatalf("unique(%s) value %T", job, opt.Value())
		}
		return d
	}
	if got := window(RegisterOptions{}, "sweep", defaultUnique); got != time.Hour {
		t.Errorf("default window %s, want an hour", got)
	}
	if got := uniqueFor(diskjobs.Schedule{Every: 10 * time.Minute}); got != 10*time.Minute {
		t.Errorf("an interval schedule's default %s", got)
	}
	o := RegisterOptions{Unique: func(job string) time.Duration {
		if job == "export" {
			return 6 * time.Hour
		}
		return 0
	}}
	if got := window(o, "export", defaultUnique); got != 6*time.Hour {
		t.Errorf("custom window %s", got)
	}
	if got := window(o, "sweep", 15*time.Minute); got != 15*time.Minute {
		t.Errorf("a zero return gave %s, want the fallback", got)
	}
	for _, d := range []time.Duration{-time.Minute, 500 * time.Millisecond} {
		bad := RegisterOptions{Unique: func(string) time.Duration { return d }}
		if _, err := bad.unique("sweep", defaultUnique); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("window %s: %v", d, err)
		}
	}
}
