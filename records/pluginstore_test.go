package records_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/acl"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/locks"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/filesystem/records/share"
	"github.com/ubgo/filesystem/records/versions"
	"github.com/ubgo/filesystem/records/webhooks"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// customMark is in every ID the test's generator makes, so a row created
// by the default generator (prefix plus random base32) cannot pass.
const customMark = "-app-"

// pluginEnv is a Store running every plugin that creates rows of its own,
// with an application ID scheme (WithIDs) and clock (WithClock), and no
// per-plugin WithClock.
type pluginEnv struct {
	store   *records.Store
	now     time.Time
	acl     *acl.Plugin
	folders *folders.Plugin
	vers    *versions.Plugin
	share   *share.Plugin
	hooks   *webhooks.Plugin
	quota   *quota.Plugin
	locks   *locks.Plugin
	file    records.File
}

var textPolicy = upload.Policy{MaxSize: 1 << 10, Allowed: []string{"text/plain"}}

func newPluginEnv(t *testing.T) *pluginEnv {
	t.Helper()
	ctx := t.Context()
	// Far from the wall clock, so a plugin reading time.Now cannot pass.
	e := &pluginEnv{now: time.Date(2040, 1, 2, 3, 4, 5, 0, time.UTC)}
	var n atomic.Int64
	ids := func(prefix string) string { return fmt.Sprintf("%s%s%06d", prefix, customMark, n.Add(1)) }
	var err error
	e.acl, e.folders, e.share, e.locks = acl.New(), folders.New(), share.New(), locks.New()
	if e.vers, err = versions.New(); err != nil {
		t.Fatal(err)
	}
	if e.hooks, err = webhooks.New(); err != nil {
		t.Fatal(err)
	}
	if e.quota, err = quota.New(func(context.Context, string) (quota.Limit, error) {
		return quota.Limit{Bytes: 1 << 20, Files: 100}, nil
	}); err != nil {
		t.Fatal(err)
	}
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	e.store, err = records.New(testdb.Open(t), testdb.Dialect(), mgr,
		records.WithIDs(ids), records.WithClock(func() time.Time { return e.now }),
		records.WithPlugins(e.acl, e.folders, e.vers, e.share, e.hooks, e.quota, e.locks))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.hooks.AddEndpoint(ctx, e.store, "t", "https://hooks.example.com/in"); err != nil {
		t.Fatal(err)
	}
	if e.file, err = e.store.Upload(ctx, "t", "media", strings.NewReader("one"), "a.txt", textPolicy); err != nil {
		t.Fatal(err)
	}
	return e
}

// TestPluginsUseStoreIDs: every row a plugin creates takes its ID from the
// Store's generator (WithIDs, through Store.NewID), with the plugin's own
// prefix.
func TestPluginsUseStoreIDs(t *testing.T) {
	ctx := t.Context()
	e := newPluginEnv(t)
	s := e.store
	cases := map[string]struct {
		prefix string
		id     func(t *testing.T) string
	}{
		"acl grant": {acl.IDPrefix, func(t *testing.T) string {
			g, err := e.acl.Grant(ctx, s, "t", acl.Grant{Subject: "user:a", Kind: acl.OnFile, Resource: e.file.ID, Level: acl.Read})
			must(t, err)
			return g.ID
		}},
		"folder": {folders.IDPrefix, func(t *testing.T) string {
			f, err := e.folders.Create(ctx, s, "t", "", "docs")
			must(t, err)
			return f.ID
		}},
		"version": {versions.IDPrefix, func(t *testing.T) string {
			_, err := s.Replace(ctx, "t", e.file.ID, strings.NewReader("two"), "a.txt", textPolicy)
			must(t, err)
			list, err := e.vers.Versions(ctx, s, "t", e.file.ID)
			must(t, err)
			return list[0].ID
		}},
		"share link": {share.IDPrefix, func(t *testing.T) string {
			sh, err := e.share.Create(ctx, s, "t", e.file.ID, share.Options{TTL: time.Hour})
			must(t, err)
			return sh.ID
		}},
		"share use": {"sev_", func(t *testing.T) string {
			sh, err := e.share.Create(ctx, s, "t", e.file.ID, share.Options{TTL: time.Hour})
			must(t, err)
			_, err = e.share.Resolve(ctx, s, sh.Token)
			must(t, err)
			var id string
			must(t, s.One(ctx, sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(share.EventID, &id)).From(share.EventsTable).Where(sqlb.Eq(share.EventShareID, sh.ID))))
			return id
		}},
		"webhook endpoint": {webhooks.EndpointIDPrefix, func(t *testing.T) string {
			ep, err := e.hooks.AddEndpoint(ctx, s, "t", "https://other.example.com/in")
			must(t, err)
			return ep.ID
		}},
		"webhook delivery": {webhooks.DeliveryIDPrefix, func(t *testing.T) string {
			list, err := e.hooks.Deliveries(ctx, s, "t", webhooks.StatePending)
			must(t, err)
			return list[0].ID
		}},
		"quota reservation": {quota.ReservationIDPrefix, func(t *testing.T) string {
			r, err := e.quota.Reserve(ctx, s, "t", 10, time.Hour)
			must(t, err)
			return r.ID
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if id := tc.id(t); !strings.HasPrefix(id, tc.prefix+customMark) {
				t.Fatalf("ID = %q, want one from WithIDs starting %q", id, tc.prefix+customMark)
			}
		})
	}
}

// TestPluginsUseStoreClock: a plugin built without its own WithClock
// stamps and decides time by the Store's clock (records.WithClock).
func TestPluginsUseStoreClock(t *testing.T) {
	ctx := t.Context()
	e := newPluginEnv(t)
	s, at := e.store, e.now

	g, err := e.acl.Grant(ctx, s, "t", acl.Grant{Subject: "user:a", Kind: acl.OnFile, Resource: e.file.ID, Level: acl.Read})
	must(t, err)
	f, err := e.folders.Create(ctx, s, "t", "", "docs")
	must(t, err)
	_, err = s.Replace(ctx, "t", e.file.ID, strings.NewReader("two"), "a.txt", textPolicy)
	must(t, err)
	vs, err := e.vers.Versions(ctx, s, "t", e.file.ID)
	must(t, err)
	sh, err := e.share.Create(ctx, s, "t", e.file.ID, share.Options{TTL: time.Hour})
	must(t, err)
	l, err := e.locks.Lock(ctx, s, "t", e.file.ID, "user:a", time.Minute)
	must(t, err)
	var stamped time.Time
	must(t, s.One(ctx, sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(quota.UpdatedAt, &stamped)).From(quota.Table).Where(sqlb.Eq(quota.Scope, "t"))))
	for name, got := range map[string]time.Time{
		"acl grant created": g.CreatedAt, "folder created": f.CreatedAt, "version replaced": vs[0].ReplacedAt,
		"share created": sh.CreatedAt, "share expires": sh.ExpiresAt.Add(-time.Hour),
		"lock taken": l.LockedAt, "lock expires": l.ExpiresAt.Add(-time.Minute), "quota usage stamped": stamped,
	} {
		if !got.Equal(at) {
			t.Errorf("%s at %v, want the Store's clock %v", name, got, at)
		}
	}

	// Expiry follows the Store's clock: past the lock's TTL and the
	// link's, both are gone.
	e.now = at.Add(2 * time.Hour)
	if _, held, err := e.locks.LockOf(ctx, s, "t", e.file.ID); err != nil || held {
		t.Errorf("lock after its TTL by the Store's clock: held %v, %v", held, err)
	}
	if _, err := e.share.Resolve(ctx, s, sh.Token); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("link after its TTL by the Store's clock = %v, want not found", err)
	}
}

// TestPluginClockOverride: a plugin's own WithClock still wins over the
// Store's, for quota (which had none before) as for the others.
func TestPluginClockOverride(t *testing.T) {
	ctx := t.Context()
	own := time.Date(2031, 5, 6, 7, 8, 9, 0, time.UTC)
	q, err := quota.New(func(context.Context, string) (quota.Limit, error) {
		return quota.Limit{Bytes: 1 << 20, Files: 100}, nil
	}, quota.WithClock(func() time.Time { return own }))
	must(t, err)
	a := acl.New(acl.WithClock(func() time.Time { return own }))
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	s, err := records.New(testdb.Open(t), testdb.Dialect(), mgr, records.WithPlugins(q, a), records.WithClock(func() time.Time { return own.AddDate(5, 0, 0) }))
	must(t, err)
	_, err = s.Migrate(ctx)
	must(t, err)
	file, err := s.Upload(ctx, "t", "media", strings.NewReader("one"), "a.txt", textPolicy)
	must(t, err)
	var stamped time.Time
	must(t, s.One(ctx, sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(quota.UpdatedAt, &stamped)).From(quota.Table).Where(sqlb.Eq(quota.Scope, "t"))))
	g, err := a.Grant(ctx, s, "t", acl.Grant{Subject: "user:a", Kind: acl.OnFile, Resource: file.ID, Level: acl.Read})
	must(t, err)
	if !stamped.Equal(own) || !g.CreatedAt.Equal(own) {
		t.Fatalf("quota stamped %v, grant created %v; want the plugins' own clock %v", stamped, g.CreatedAt, own)
	}
}

// must stops the test on err.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
