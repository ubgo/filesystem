// Command records-egress shows the egress plugin of the records layer on
// SQLite and an in-memory disk: every link the Store issues and every
// stream an app reports with Store.Served adds bytes to its scope's
// bucket for the UTC day; Days and Total read a period; Report turns
// finished days into egress.daily_total events for billing, exactly once
// per growth; Prune drops reported days. A fixed clock moves the demo
// from one day to the next.
//
//	go run ./records-egress
//
// It prints what each step returned; main_test.go runs the same steps.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/egress"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	diskName = "media"
	tenant   = "acme"
	linkTTL  = 10 * time.Minute
)

// clock is the Store's clock, moved by the demo from day to day.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Set(t time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.now = t }

// billing collects the egress.daily_total events, as a billing consumer
// behind webhooks would. It keeps the IDs it has seen, so a repeated
// delivery is counted once.
type billing struct {
	seen  map[string]bool
	lines []string
}

func (b *billing) Name() string { return "billing" }

func (b *billing) OnEvent(_ context.Context, _ *records.Store, e records.Event) error {
	if e.Kind != egress.EventDailyTotal || b.seen[e.ID] {
		return nil
	}
	b.seen[e.ID] = true
	b.lines = append(b.lines, fmt.Sprintf("%s +%s bytes +%s requests", e.Detail[egress.DetailDay], e.Detail[egress.DetailBytes], e.Detail[egress.DetailRequests]))
	return nil
}

// newStore installs the egress plugin. The disk signs URLs, so URL and
// DownloadURL issue links, and each link is metered.
// ds:def id=newegressstore-n7c3w9kp
func newStore(ctx context.Context, db *sql.DB, now func() time.Time, plugins ...records.Plugin) (*records.Store, *egress.Plugin, error) {
	signer, err := signedurl.New("https://app.example.com/files", []signedurl.Key{{ID: "k1", Secret: []byte("0123456789abcdef0123456789abcdef")}})
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskName, memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	eg := egress.New()
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithClock(now), records.WithPlugins(append([]records.Plugin{eg}, plugins...)...))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, eg, nil
}

// streamed reports bytes the app sent itself (a proxy route, a range
// response), which no link accounts for. Call it once the response is
// written, with the bytes actually sent.
// ds:def id=streamed-h5q2m8vx
func streamed(ctx context.Context, s *records.Store, f records.File, n int64) error {
	return s.Served(ctx, records.Served{Kind: records.ServeStream, Scope: f.Scope, FileID: f.ID, Bytes: n})
}

// usage reads the scope's buckets and their total for a period: the UTC
// days from from's day up to, not including, to's day.
// ds:def id=usage-t9w4c2hn
func usage(ctx context.Context, s *records.Store, eg *egress.Plugin, from, to time.Time) ([]egress.Bucket, egress.Bucket, error) {
	days, err := eg.Days(ctx, s, tenant, from, to)
	if err != nil {
		return nil, egress.Bucket{}, err
	}
	total, err := eg.Total(ctx, s, tenant, from, to)
	return days, total, err
}

// run performs every step and prints what it got.
func run(ctx context.Context, w io.Writer, dir string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "records.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	day1 := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	c := &clock{now: day1}
	bill := &billing{seen: map[string]bool{}}
	s, eg, err := newStore(ctx, db, c.Now, bill)
	if err != nil {
		return err
	}
	f, err := record(ctx, s, "report.pdf", strings.Repeat("x", 1000))
	if err != nil {
		return err
	}
	for range 2 {
		if _, err := s.URL(ctx, f, linkTTL); err != nil {
			return err
		}
	}
	if err := streamed(ctx, s, f, 400); err != nil {
		return err
	}
	c.Set(day1.Add(24 * time.Hour))
	if _, err := s.DownloadURL(ctx, f, linkTTL); err != nil {
		return err
	}
	days, total, err := usage(ctx, s, eg, day1, day1.Add(72*time.Hour))
	if err != nil {
		return err
	}
	for _, d := range days {
		fmt.Fprintf(w, "%s %d bytes in %d requests\n", d.Day, d.Bytes, d.Requests)
	}
	fmt.Fprintf(w, "total %d bytes in %d requests\n", total.Bytes, total.Requests)
	n, err := eg.Report(ctx, s, c.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "report on day 2: %d event(s)\n", n)
	if _, err := s.URL(ctx, f, linkTTL); err != nil {
		return err
	}
	c.Set(day1.Add(48 * time.Hour))
	for _, run := range []string{"first", "second"} {
		n, err := eg.Report(ctx, s, c.Now())
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "report on day 3 (%s run): %d event(s)\n", run, n)
	}
	fmt.Fprintf(w, "billed: %s\n", strings.Join(bill.lines, "; "))
	pruned, err := eg.Prune(ctx, s, c.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "pruned %d reported day(s)\n", pruned)
	if err := zones(ctx, w, dir); err != nil {
		return err
	}
	return perLink(ctx, w, dir)
}

// record stores body and records it as a file of the tenant.
func record(ctx context.Context, s *records.Store, key, body string) (records.File, error) {
	disk, err := s.Disks().Disk(diskName)
	if err != nil {
		return records.File{}, err
	}
	if _, err := disk.Write(ctx, key, strings.NewReader(body), filesystem.WriteOptions{}); err != nil {
		return records.File{}, err
	}
	return s.CreateFile(ctx, records.NewFile{Scope: tenant, Disk: diskName, Key: key})
}

func main() {
	dir, err := os.MkdirTemp("", "records-egress")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
