package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/versions"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Plans of the history demo: the free plan keeps the last version, pro
// keeps ten for at most 30 days. The app's billing table would hold them.
var historyPlans = map[string]versions.KeepPolicy{
	"free-co": {Count: 1},
	"pro-co":  {Count: 10, MaxAge: 30 * 24 * time.Hour},
}

// planClock is a settable clock, so the demo can age the versions.
type planClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *planClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *planClock) Add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }

// planKeep is the versions.KeepFunc: each file keeps what its scope's
// plan allows. A scope without a plan is an error, which refuses the
// replacement (it rolls back) rather than keeping history unbounded.
// ds:def id=plankeep-x3n7q2vd
func planKeep(_ context.Context, req versions.KeepRequest) (versions.KeepPolicy, error) {
	pol, ok := historyPlans[req.File.Scope]
	if !ok {
		return versions.KeepPolicy{}, filesystem.Errorf(filesystem.CodePermission, "no plan for scope %q", req.File.Scope)
	}
	return pol, nil
}

// newPlanStore installs versions with a policy per file and restored
// copies kept beside the file, inside the prefix its tenant owns.
// ds:def id=newplanstore-k8w4t6mb
func newPlanStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager, now func() time.Time) (*records.Store, *versions.Plugin, error) {
	v, err := versions.New(versions.WithKeepFunc(planKeep), versions.WithRestoreKey(versions.RestoreBesideFile))
	if err != nil {
		return nil, nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithVisibilityDisks(publicDisk, privateDisk), records.WithClock(now), records.WithPlugins(v))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, v, nil
}

// tenantPolicy stores a scope's files under its own prefix, the layout
// RestoreBesideFile keeps restored copies inside.
func tenantPolicy(scope string) upload.Policy {
	p := textPolicy
	p.Key = upload.RandomKey("tenants/" + scope)
	return p
}

// planHistory edits one file per plan three times, trims after 40 days,
// and restores the free tenant's version beside its file.
func planHistory(ctx context.Context, w io.Writer, dir string) error {
	db, err := openSQLite(filepath.Join(dir, "plans.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	disks, err := newDisks()
	if err != nil {
		return err
	}
	clock := &planClock{now: time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)}
	s, v, err := newPlanStore(ctx, db, disks, clock.Now)
	if err != nil {
		return err
	}
	files := map[string]records.File{}
	for _, scope := range []string{"free-co", "pro-co"} {
		f, err := s.Upload(ctx, scope, "", strings.NewReader("v0"), "plan.txt", tenantPolicy(scope))
		if err != nil {
			return err
		}
		for _, body := range []string{"v1", "v2", "v3"} {
			if f, err = s.Replace(ctx, scope, f.ID, strings.NewReader(body), "", tenantPolicy(scope)); err != nil {
				return err
			}
		}
		list, err := v.Versions(ctx, s, scope, f.ID)
		if err != nil {
			return err
		}
		files[scope] = f
		fmt.Fprintf(w, "%s after 3 edits keeps %d version(s)\n", scope, len(list))
	}
	clock.Add(40 * 24 * time.Hour)
	dropped, err := v.Trim(ctx, s, "pro-co", files["pro-co"].ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Trim 40 days later drops %d pro-co version(s) past 30 days\n", dropped)
	list, err := v.Versions(ctx, s, "free-co", files["free-co"].ID)
	if err != nil {
		return err
	}
	restored, err := v.Restore(ctx, s, "free-co", list[0].ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "restored copy stays in the tenant's prefix: %t\n", strings.HasPrefix(restored.Key, "tenants/free-co/versions/"))
	return nil
}
