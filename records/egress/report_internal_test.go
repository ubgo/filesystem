package egress

import (
	"context"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
)

// counter counts the daily_total events that commit.
type counter struct{ n int }

func (c *counter) Name() string { return "billing" }

func (c *counter) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	if e.Kind == EventDailyTotal {
		s.AfterCommit(ctx, func(context.Context) { c.n++ })
	}
	return nil
}

// TestRacingReportsEmitOnce: two Report runs read the same bucket before
// either moved its mark. Each then reports what it read; only the first
// may emit, the second must find the mark moved and emit nothing.
func TestRacingReportsEmitOnce(t *testing.T) {
	ctx := t.Context()
	p, c := New(), &counter{}
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	day := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	s, err := records.New(testdb.Open(t), testdb.Dialect(), mgr, records.WithPlugins(p, c), records.WithClock(func() time.Time { return day }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Served(ctx, records.Served{Kind: records.ServeLink, Scope: "a", Bytes: 10}); err != nil {
		t.Fatal(err)
	}
	read := Bucket{Scope: "a", Day: day.Format(DayLayout), Bytes: 10, Requests: 1}
	first, err := p.report(ctx, s, read, Bucket{})
	if err != nil || !first {
		t.Fatalf("first run = %v, %v", first, err)
	}
	second, err := p.report(ctx, s, read, Bucket{})
	if err != nil || second || c.n != 1 {
		t.Fatalf("second run = %v, %v; %d events", second, err, c.n)
	}
}
