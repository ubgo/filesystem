package river_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/jobs/diskjobs"
	fsriver "github.com/ubgo/filesystem/jobs/river"
)

func job(name string, count *int64, mu *sync.Mutex) diskjobs.Job {
	return diskjobs.Func(name, func(context.Context, time.Time) (diskjobs.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		*count++
		return diskjobs.Result{Count: *count}, nil
	})
}

func TestNewWorkerValidation(t *testing.T) {
	ok := diskjobs.Func("a", func(context.Context, time.Time) (diskjobs.Result, error) { return diskjobs.Result{}, nil })
	for name, list := range map[string][]diskjobs.Job{
		"no name": {{Run: ok.Run}},
		"no run":  {{Name: "x"}},
		"twice":   {ok, ok},
	} {
		if _, err := fsriver.NewWorker(fsriver.WorkerOptions{}, list...); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestWorkRunsByName(t *testing.T) {
	var mu sync.Mutex
	var n int64
	var reports []diskjobs.Report
	fixed := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	failing := diskjobs.Func("failing", func(context.Context, time.Time) (diskjobs.Result, error) {
		return diskjobs.Result{}, filesystem.ErrUnavailable
	})
	w, err := fsriver.NewWorker(fsriver.WorkerOptions{OnRun: func(r diskjobs.Report) { reports = append(reports, r) }, Now: func() time.Time { return fixed }}, job("count", &n, &mu), failing)
	if err != nil {
		t.Fatal(err)
	}
	run := func(name string) error {
		return w.Work(t.Context(), &river.Job[fsriver.Args]{JobRow: &rivertype.JobRow{}, Args: fsriver.Args{Job: name}})
	}
	if err := run("count"); err != nil || n != 1 {
		t.Errorf("count: %v, ran %d", err, n)
	}
	if err := run("failing"); !errors.Is(err, filesystem.ErrUnavailable) {
		t.Errorf("failing job: %v", err)
	}
	var cancel *rivertype.JobCancelError
	if err := run("unknown"); !errors.As(err, &cancel) || filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("unknown job: %v", err)
	}
	if len(reports) != 2 || reports[0].Job != "count" || reports[0].Result.Count != 1 || !reports[0].Started.Equal(fixed) || reports[1].Err == nil {
		t.Errorf("reports %+v", reports)
	}
	// The zero options work too.
	plain, _ := fsriver.NewWorker(fsriver.WorkerOptions{}, job("count", &n, &mu))
	if err := plain.Work(t.Context(), &river.Job[fsriver.Args]{JobRow: &rivertype.JobRow{}, Args: fsriver.Args{Job: "count"}}); err != nil {
		t.Error(err)
	}
}

func TestPeriodicAndInsertOpts(t *testing.T) {
	ok := diskjobs.Func("a", func(context.Context, time.Time) (diskjobs.Result, error) { return diskjobs.Result{}, nil })
	if got := fsriver.Periodic(diskjobs.Schedule{Job: ok, Every: time.Hour}, diskjobs.Schedule{Job: ok.Named("b"), Every: time.Minute, RunAtStart: true}); len(got) != 2 {
		t.Fatalf("periodic jobs %d", len(got))
	}
	o := fsriver.InsertOpts()
	if !o.UniqueOpts.ByArgs || len(o.UniqueOpts.ByState) == 0 {
		t.Errorf("insert opts %+v", o)
	}
	if (fsriver.Args{}).Kind() != fsriver.Kind {
		t.Error("kind")
	}
}

// EnvPostgresDSN names the Postgres the integration test uses; it creates
// and drops a schema of its own there.
const EnvPostgresDSN = "FILESYSTEM_RIVER_PG_DSN"

// TestOnPostgres runs maintenance jobs through a real River client: a
// periodic job that runs at start, and an enqueued one that is single-flight.
func TestOnPostgres(t *testing.T) {
	dsn := os.Getenv(EnvPostgresDSN)
	if dsn == "" {
		t.Skipf("set %s to a Postgres DSN (task test:river) to run River end to end", EnvPostgresDSN)
	}
	ctx := t.Context()
	schema := fmt.Sprintf("river_test_%d", rand.Int64())
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.WithoutCancel(ctx), "DROP SCHEMA "+schema+" CASCADE") })
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var started, manual int64
	reports := make(chan diskjobs.Report, 10)
	worker, err := fsriver.NewWorker(fsriver.WorkerOptions{OnRun: func(r diskjobs.Report) { reports <- r }}, job("at-start", &started, &mu), job("manual", &manual, &mu))
	if err != nil {
		t.Fatal(err)
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:       map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 2}},
		Workers:      workers,
		PeriodicJobs: fsriver.Periodic(diskjobs.Schedule{Job: job("at-start", &started, &mu), Every: time.Hour, RunAtStart: true}),
		Schema:       schema,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Two inserts before the client runs: the second is a duplicate.
	if err := fsriver.Enqueue(ctx, client, "manual"); err != nil {
		t.Fatal(err)
	}
	res, err := client.Insert(ctx, fsriver.Args{Job: "manual"}, fsriver.InsertOpts())
	if err != nil || !res.UniqueSkippedAsDuplicate {
		t.Fatalf("second insert: %+v, %v", res, err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Stop(context.WithoutCancel(ctx)) }()
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
	mu.Lock()
	defer mu.Unlock()
	if manual != 1 {
		t.Errorf("manual ran %d times, want once", manual)
	}
}
