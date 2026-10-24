package asynq_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/ubgo/filesystem"
	fsasynq "github.com/ubgo/filesystem/jobs/asynq"
	"github.com/ubgo/filesystem/jobs/diskjobs"
)

func job(name string, count *int64, mu *sync.Mutex) diskjobs.Job {
	return diskjobs.Func(name, func(context.Context, time.Time) (diskjobs.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		*count++
		return diskjobs.Result{Count: *count}, nil
	})
}

func TestNewHandlerValidation(t *testing.T) {
	ok := diskjobs.Func("a", func(context.Context, time.Time) (diskjobs.Result, error) { return diskjobs.Result{}, nil })
	for name, list := range map[string][]diskjobs.Job{
		"no name": {{Run: ok.Run}},
		"no run":  {{Name: "x"}},
		"twice":   {ok, ok},
	} {
		if _, err := fsasynq.NewHandler(fsasynq.HandlerOptions{}, list...); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestProcessTaskRunsByName(t *testing.T) {
	var mu sync.Mutex
	var n int64
	var reports []diskjobs.Report
	fixed := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	failing := diskjobs.Func("failing", func(context.Context, time.Time) (diskjobs.Result, error) {
		return diskjobs.Result{}, filesystem.ErrUnavailable
	})
	h, err := fsasynq.NewHandler(fsasynq.HandlerOptions{OnRun: func(r diskjobs.Report) { reports = append(reports, r) }, Now: func() time.Time { return fixed }}, job("count", &n, &mu), failing)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.ProcessTask(t.Context(), fsasynq.NewTask("count")); err != nil || n != 1 {
		t.Errorf("count: %v, ran %d", err, n)
	}
	if err := h.ProcessTask(t.Context(), fsasynq.NewTask("failing")); !errors.Is(err, filesystem.ErrUnavailable) {
		t.Errorf("failing: %v", err)
	}
	if err := h.ProcessTask(t.Context(), fsasynq.NewTask("unknown")); !errors.Is(err, asynq.SkipRetry) || filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("unknown: %v", err)
	}
	if len(reports) != 2 || reports[0].Result.Count != 1 || !reports[0].Started.Equal(fixed) || reports[1].Err == nil {
		t.Errorf("reports %+v", reports)
	}
	plain, _ := fsasynq.NewHandler(fsasynq.HandlerOptions{}, job("count", &n, &mu))
	if err := plain.ProcessTask(t.Context(), fsasynq.NewTask("count")); err != nil {
		t.Error(err)
	}
	if fsasynq.NewTask("x").Type() != fsasynq.TaskType {
		t.Error("task type")
	}
}

// EnvRedisAddr names the Redis the integration test uses (host:port). It
// enqueues to Asynq's default queue there, so point it at a throwaway
// server (task test:asynq starts one), never a shared one.
const EnvRedisAddr = "FILESYSTEM_ASYNQ_REDIS_ADDR"

func TestRegisterRefusesABadInterval(t *testing.T) {
	// No Redis is contacted before the interval check.
	sched := asynq.NewScheduler(asynq.RedisClientOpt{Addr: "127.0.0.1:1"}, nil)
	ok := diskjobs.Func("a", func(context.Context, time.Time) (diskjobs.Result, error) { return diskjobs.Result{}, nil })
	if err := fsasynq.Register(t.Context(), sched, nil, diskjobs.Schedule{Job: ok}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("zero interval: %v", err)
	}
	// A run at start that cannot be enqueued fails the registration.
	unreachable := asynq.NewClient(asynq.RedisClientOpt{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond})
	t.Cleanup(func() { _ = unreachable.Close() })
	if err := fsasynq.Register(t.Context(), sched, unreachable, diskjobs.Schedule{Job: ok, Every: time.Hour, RunAtStart: true}); err == nil {
		t.Error("a failed enqueue at start was not reported")
	}
}

// TestOnRedis runs maintenance jobs through a real Asynq server: one at
// start through Register, one enqueued twice, which runs once.
func TestOnRedis(t *testing.T) {
	addr := os.Getenv(EnvRedisAddr)
	if addr == "" {
		t.Skipf("set %s to a throwaway Redis (task test:asynq) to run Asynq end to end", EnvRedisAddr)
	}
	ctx := t.Context()
	redis := asynq.RedisClientOpt{Addr: addr}
	client := asynq.NewClient(redis)
	t.Cleanup(func() { _ = client.Close() })

	var mu sync.Mutex
	var started, manual int64
	reports := make(chan diskjobs.Report, 10)
	handler, err := fsasynq.NewHandler(fsasynq.HandlerOptions{OnRun: func(r diskjobs.Report) { reports <- r }}, job("at-start", &started, &mu), job("manual", &manual, &mu))
	if err != nil {
		t.Fatal(err)
	}
	sched := asynq.NewScheduler(redis, &asynq.SchedulerOpts{})
	if err := fsasynq.Register(ctx, sched, client, diskjobs.Schedule{Job: job("at-start", &started, &mu), Every: time.Hour, RunAtStart: true}); err != nil {
		t.Fatal(err)
	}
	if err := fsasynq.Enqueue(ctx, client, "manual"); err != nil {
		t.Fatal(err)
	}
	if err := fsasynq.Enqueue(ctx, client, "manual"); err != nil {
		t.Fatalf("duplicate enqueue: %v", err)
	}
	mux := asynq.NewServeMux()
	mux.Handle(fsasynq.TaskType, handler)
	srv := asynq.NewServer(redis, asynq.Config{Concurrency: 2, LogLevel: asynq.ErrorLevel})
	if err := srv.Start(mux); err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	seen := map[string]bool{}
	timeout := time.After(30 * time.Second)
	for len(seen) < 2 {
		select {
		case r := <-reports:
			if r.Err != nil {
				t.Fatalf("report %+v", r)
			}
			seen[r.Job] = true
		case <-timeout:
			t.Fatalf("only %v ran", seen)
		}
	}
	time.Sleep(200 * time.Millisecond) // a duplicate, had it been queued, would run now
	mu.Lock()
	defer mu.Unlock()
	if manual != 1 {
		t.Errorf("manual ran %d times, want once", manual)
	}
}
