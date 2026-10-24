package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/egress"
	"github.com/ubgo/filesystem/records/share"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/sqlb"
)

// shareTTL is how long the demo's share links stay usable.
const shareTTL = 7 * 24 * time.Hour

// newPerLinkStore installs egress with WithPerSubject(records.ServeShare)
// beside the share plugin: the share handler reports every download with
// the link's ID as the subject, so each link gets its own daily buckets
// as well as counting in the scope's.
// ds:def id=newperlinkstore-w6d3k8qz
func newPerLinkStore(ctx context.Context, db *sql.DB, now func() time.Time) (*records.Store, *egress.Plugin, *share.Plugin, error) {
	signer, err := signedurl.New("https://app.example.com/files", []signedurl.Key{{ID: "k1", Secret: []byte("0123456789abcdef0123456789abcdef")}})
	if err != nil {
		return nil, nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskName, memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, nil, err
	}
	eg := egress.New(egress.WithPerSubject(records.ServeShare))
	sp := share.New()
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithClock(now), records.WithPlugins(eg, sp))
	if err != nil {
		return nil, nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, nil, err
	}
	return s, eg, sp, nil
}

// linkUsage is what one share link carried over a period: "this link
// used 2 GB this month", or the input of a cap your handler enforces.
// ds:def id=linkusage-b5m2v9xt
func linkUsage(ctx context.Context, s *records.Store, eg *egress.Plugin, link share.Share, from, to time.Time) (bytes, requests int64, err error) {
	days, err := eg.SubjectDays(ctx, s, tenant, records.ServeShare, link.ID, from, to)
	if err != nil {
		return 0, 0, err
	}
	for _, d := range days {
		bytes += d.Bytes
		requests += d.Requests
	}
	return bytes, requests, nil
}

// perLink shares one file through two links, downloads through the share
// handler, and reads each link's bytes and the scope's.
func perLink(ctx context.Context, w io.Writer, dir string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "shares.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	day := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	c := &clock{now: day}
	s, eg, sp, err := newPerLinkStore(ctx, db, c.Now)
	if err != nil {
		return err
	}
	f, err := record(ctx, s, "deck.pdf", strings.Repeat("x", 1000))
	if err != nil {
		return err
	}
	press, err := sp.Create(ctx, s, tenant, f.ID, share.Options{TTL: shareTTL})
	if err != nil {
		return err
	}
	partner, err := sp.Create(ctx, s, tenant, f.ID, share.Options{TTL: shareTTL})
	if err != nil {
		return err
	}
	h := share.Handler(s, sp)
	for _, token := range []string{press.Token, press.Token, partner.Token} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/"+token, nil))
		if rec.Code != http.StatusFound {
			return fmt.Errorf("share download answered %d", rec.Code)
		}
	}
	to := day.Add(24 * time.Hour)
	for _, l := range []struct {
		name string
		link share.Share
	}{{"press", press}, {"partner", partner}} {
		b, n, err := linkUsage(ctx, s, eg, l.link, day, to)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "per link: %s %d bytes in %d requests\n", l.name, b, n)
	}
	total, err := eg.Total(ctx, s, tenant, day, to)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "per link: scope %d bytes in %d requests\n", total.Bytes, total.Requests)
	return nil
}
