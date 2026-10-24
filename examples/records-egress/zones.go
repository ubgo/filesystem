package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/egress"
	"github.com/ubgo/sqlb"
)

// Two tenants of the zones demo, each billed by its own calendar day.
const (
	tokyoTenant   = "acme-tokyo"
	newYorkTenant = "globex-newyork"
)

// zoneOf is the app's tenant settings: the time zone each tenant's bill
// follows. Fixed offsets keep the demo independent of the host's zone
// database; a real app would use time.LoadLocation once and cache it.
var zoneOf = map[string]*time.Location{
	tokyoTenant:   time.FixedZone("UTC+9", 9*60*60),
	newYorkTenant: time.FixedZone("UTC-5", -5*60*60),
}

// tenantDay is the egress.BucketFunc of the zones demo: a read falls in
// the tenant's own calendar day, so a bill matches the customer's
// calendar. A tenant without a zone is metered in UTC days.
// ds:def id=tenantday-r6k3v8wq
func tenantDay(scope string, at time.Time) string {
	loc, ok := zoneOf[scope]
	if !ok {
		loc = time.UTC
	}
	return at.In(loc).Format(egress.DayLayout)
}

// newZonedStore installs the egress plugin with WithBucket(tenantDay).
// ds:def id=newzonedstore-b9t4m2xc
func newZonedStore(ctx context.Context, db *sql.DB, now func() time.Time, plugins ...records.Plugin) (*records.Store, *egress.Plugin, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	eg := egress.New(egress.WithBucket(tenantDay))
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithClock(now), records.WithPlugins(append([]records.Plugin{eg}, plugins...)...))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, eg, nil
}

// zones meters one 500-byte stream for each tenant at the same instant,
// 20:00 UTC on 3 October: already 4 October in Tokyo, still 3 October in
// New York. A report at 10:00 UTC the next day finds New York's day over
// and Tokyo's still running, so it bills New York only.
func zones(ctx context.Context, w io.Writer, dir string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "zones.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	at := time.Date(2026, 10, 3, 20, 0, 0, 0, time.UTC)
	c := &clock{now: at}
	bill := &billing{seen: map[string]bool{}}
	s, eg, err := newZonedStore(ctx, db, c.Now, bill)
	if err != nil {
		return err
	}
	disk, err := s.Disks().Disk(diskName)
	if err != nil {
		return err
	}
	for _, scope := range []string{tokyoTenant, newYorkTenant} {
		if _, err := disk.Write(ctx, scope+"/a.bin", strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
			return err
		}
		f, err := s.CreateFile(ctx, records.NewFile{Scope: scope, Disk: diskName, Key: scope + "/a.bin"})
		if err != nil {
			return err
		}
		if err := streamed(ctx, s, f, 500); err != nil {
			return err
		}
		days, err := eg.Days(ctx, s, scope, at.Add(-48*time.Hour), at.Add(48*time.Hour))
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%s: read at %s UTC lands on %s\n", scope, at.Format("2006-01-02 15:04"), days[0].Day)
	}
	c.Set(at.Add(14 * time.Hour))
	n, err := eg.Report(ctx, s, c.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "report at %s UTC: %d event(s): %s\n", c.Now().Format("2006-01-02 15:04"), n, strings.Join(bill.lines, "; "))
	return nil
}
