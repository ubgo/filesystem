package river_test

import (
	"testing"
	"time"

	"github.com/riverqueue/river"

	fsriver "github.com/ubgo/filesystem/jobs/river"
)

// TestPeriodicOptions pins the options seam: queue and attempts reach the
// insert options, uniqueness is kept whatever the options say, the zero
// value is InsertOpts, and the worker's timeout is WorkerOptions.Timeout.
func TestPeriodicOptions(t *testing.T) {
	o := fsriver.PeriodicOptions{Queue: "maintenance", MaxAttempts: 2}.InsertOpts()
	if o.Queue != "maintenance" || o.MaxAttempts != 2 || !o.UniqueOpts.ByArgs || len(o.UniqueOpts.ByState) == 0 {
		t.Fatalf("insert opts %+v", o)
	}
	if d := fsriver.InsertOpts(); d.Queue != "" || d.MaxAttempts != 0 || !d.UniqueOpts.ByArgs {
		t.Fatalf("default insert opts %+v", d)
	}
	w, err := fsriver.NewWorker(fsriver.WorkerOptions{Timeout: time.Minute})
	if err != nil || w.Timeout(nil) != time.Minute {
		t.Fatalf("timeout: %v", err)
	}
	if d, _ := fsriver.NewWorker(fsriver.WorkerOptions{}); d.Timeout(nil) != 0 {
		t.Fatal("default timeout")
	}
}

// TestWorkerTimeoutFor pins the per-job timeout seam (R-C10): TimeoutFor
// bounds the job it names, a zero return falls back to Timeout, and with
// TimeoutFor nil every job gets Timeout as before.
func TestWorkerTimeoutFor(t *testing.T) {
	job := func(name string) *river.Job[fsriver.Args] {
		return &river.Job[fsriver.Args]{Args: fsriver.Args{Job: name}}
	}
	w, err := fsriver.NewWorker(fsriver.WorkerOptions{
		Timeout: time.Minute,
		TimeoutFor: func(name string) time.Duration {
			if name == "export" {
				return 3 * time.Hour
			}
			return 0
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := w.Timeout(job("export")); got != 3*time.Hour {
		t.Errorf("export timeout %s", got)
	}
	if got := w.Timeout(job("sweep")); got != time.Minute {
		t.Errorf("a zero return gave %s, want Timeout", got)
	}
	plain, _ := fsriver.NewWorker(fsriver.WorkerOptions{Timeout: time.Minute})
	if got := plain.Timeout(job("export")); got != time.Minute {
		t.Errorf("without TimeoutFor %s", got)
	}
}
