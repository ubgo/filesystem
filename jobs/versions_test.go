package jobs_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/jobs"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/records/versions"
)

var errPlans = errors.New("plan service down")

// TestVersionsTrim: the job applies the age policy in every scope it is
// given, counts the versions dropped, and a failing scope is named
// without stopping the next.
func TestVersionsTrim(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	disk, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	mgr, _ := filesystem.NewManager(disk)
	p, err := versions.New(versions.WithClock(clock), versions.WithKeepFunc(func(_ context.Context, req versions.KeepRequest) (versions.KeepPolicy, error) {
		if req.File.Scope == "broken" && now.Hour() > 12 {
			return versions.KeepPolicy{}, errPlans
		}
		return versions.KeepPolicy{Count: 10, MaxAge: time.Hour}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	db, dialect := recordstest.Open(t)
	store, err := records.New(db, dialect, mgr, records.WithClock(clock), records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"broken", "s1", "s2"} {
		f, err := store.Upload(ctx, scope, "media", strings.NewReader(pngData), "a.png", images)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Replace(ctx, scope, f.ID, strings.NewReader(pngData+"2"), "", images); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(2 * time.Hour)
	scopes := func(context.Context) ([]string, error) { return []string{"broken", "s1", "s2"}, nil }
	j := jobs.VersionsTrim(p, store, scopes)
	res, err := run(t, j, now)
	if res.Count != 2 || !errors.Is(err, errPlans) || !strings.Contains(err.Error(), `scope "broken"`) || j.Name != jobs.NameVersionsTrim {
		t.Fatalf("trim: %+v, %v", res, err)
	}
	if res, err := run(t, jobs.VersionsTrim(p, store, func(context.Context) ([]string, error) { return []string{"s1", "s2"}, nil }), now); err != nil || res.Count != 0 {
		t.Fatalf("second run: %+v, %v", res, err)
	}
	broken := jobs.VersionsTrim(p, store, func(context.Context) ([]string, error) { return nil, filesystem.ErrUnavailable })
	if _, err := run(t, broken, now); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("scope list failure: %v", err)
	}
}
