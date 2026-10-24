package jobs_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/jobs"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/egress"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/records/webhooks"
)

// TestMeteringJobs runs each metering job through its Job value, as a
// scheduler would: a read is metered, its day reported as an event, the
// event delivered to a webhook, the delivery pruned, and an expired
// reservation swept.
func TestMeteringJobs(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	got := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { got++ }))
	defer srv.Close()
	eg := egress.New()
	wh, err := webhooks.New(webhooks.WithClient(srv.Client()), webhooks.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	q, err := quota.New(func(context.Context, string) (quota.Limit, error) { return quota.Limit{Bytes: 100}, nil })
	if err != nil {
		t.Fatal(err)
	}
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	db, dialect := recordstest.Open(t)
	s, err := records.New(db, dialect, mgr, records.WithClock(func() time.Time { return now }), records.WithPlugins(eg, wh, q))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := wh.AddEndpoint(ctx, s, "t", srv.URL, egress.EventDailyTotal); err != nil {
		t.Fatal(err)
	}
	if err := s.Served(ctx, records.Served{Kind: records.ServeLink, Scope: "t", Bytes: 9}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Reserve(ctx, s, "t", 50, time.Minute); err != nil {
		t.Fatal(err)
	}

	tomorrow := now.Add(24 * time.Hour)
	for _, step := range []struct {
		job  jobs.Job
		at   time.Time
		want int64
	}{
		{jobs.EgressReport(eg, s), tomorrow, 1},
		{jobs.WebhookDeliver(wh, s), tomorrow, 1},
		{jobs.WebhookPrune(wh, s, time.Hour), tomorrow.Add(2 * time.Hour), 1},
		{jobs.QuotaSweepReservations(q, s), now.Add(time.Minute), 1},
	} {
		res, err := step.job.Run(ctx, step.at)
		if err != nil || res.Count != step.want {
			t.Fatalf("%s = %+v, %v; want %d", step.job.Name, res, err, step.want)
		}
	}
	if got != 1 {
		t.Fatalf("webhook received %d deliveries", got)
	}
	if _, err := jobs.WebhookPrune(wh, s, 0).Run(ctx, now); err == nil {
		t.Fatal("a zero retention was accepted")
	}
}
