package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// Plans of the retention-by-plan demo: how long each plan keeps chat
// attachments. The app's billing table would hold these.
const (
	freeKeep = 30 * 24 * time.Hour
	proKeep  = 365 * 24 * time.Hour
)

// tenantPlan is one row of the app's billing table.
type tenantPlan struct {
	Scope string
	Pro   bool
}

// planTenants are the tenants the demo sweeps, from the app's billing.
var planTenants = []tenantPlan{{Scope: "free-co"}, {Scope: "pro-co", Pro: true}}

// sweepByPlan applies each tenant's own retention: SweepRetentionIn is
// SweepRetention limited to one scope, with the rules the caller passes.
// The Store has no WithRetention of its own for chat, so nothing applies
// to every tenant on top.
// ds:def id=sweepbyplan-f8c2n5tj
func sweepByPlan(ctx context.Context, s *records.Store, tenants []tenantPlan, now time.Time) (map[string]records.RetentionReport, error) {
	out := map[string]records.RetentionReport{}
	for _, t := range tenants {
		keep := freeKeep
		if t.Pro {
			keep = proKeep
		}
		rules := []records.Retention{{OwnerType: ownerChat, MaxAge: keep}}
		rep, err := s.SweepRetentionIn(ctx, t.Scope, rules, now, false)
		if err != nil {
			return out, fmt.Errorf("retention of %s: %w", t.Scope, err)
		}
		out[t.Scope] = rep
	}
	return out, nil
}

// newPlanStore registers the chat owner type without a global rule.
func newPlanStore(ctx context.Context, db *sql.DB, clock *Clock) (*records.Store, error) {
	disks, err := newDisks()
	if err != nil {
		return nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(ownerUser, ownerChat), records.WithClock(clock.Now))
	if err != nil {
		return nil, err
	}
	_, err = s.Migrate(ctx)
	return s, err
}

// planDemo gives each tenant a chat attachment, then sweeps 60 days
// later: the free plan's 30 days have passed, the pro plan's year has not.
func planDemo(ctx context.Context, w io.Writer, dir string) error {
	db, err := openSQLite(filepath.Join(dir, "plans.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	clock := &Clock{now: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)}
	s, err := newPlanStore(ctx, db, clock)
	if err != nil {
		return err
	}
	for _, t := range planTenants {
		f, err := s.Upload(ctx, t.Scope, diskName, strings.NewReader("photo"), "photo.txt", textPolicy)
		if err != nil {
			return err
		}
		if _, err := s.Attach(ctx, t.Scope, f.ID, records.Owner{Type: ownerChat, ID: "c1"}, "attachment"); err != nil {
			return err
		}
	}
	reports, err := sweepByPlan(ctx, s, planTenants, clock.Now().Add(60*24*time.Hour))
	if err != nil {
		return err
	}
	for _, t := range planTenants {
		r := reports[t.Scope]
		fmt.Fprintf(w, "retention by plan, %s after 60 days: detached %d, purged %d\n", t.Scope, len(r.Detached), len(r.Purged))
	}
	return nil
}
