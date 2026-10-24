package quota_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

var anyPolicy = upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}}

var errOrgs = errors.New("org directory down")

// orgOf charges workspaces w1 and w2 to one organisation capped at 100
// bytes and 3 files, and every file to a size class that changes when a
// replacement crosses 50 bytes.
func orgOf(_ context.Context, f records.File) ([]quota.Dimension, error) {
	class := "class:small"
	if f.Size > 50 {
		class = "class:big"
	}
	return []quota.Dimension{{Key: "org:acme", Limit: quota.Limit{Bytes: 100, Files: 3}}, {Key: class}}, nil
}

func dimUsage(t *testing.T, e *env, key string) quota.Usage {
	t.Helper()
	u, err := e.plugin.DimensionUsage(t.Context(), e.store, key)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (e *env) replaceWith(t *testing.T, scope, id string, size int) error {
	t.Helper()
	_, err := e.store.Replace(t.Context(), scope, id, strings.NewReader(strings.Repeat("y", size)), "", anyPolicy)
	return err
}

// TestDimensionsCapAcrossScopes: one dimension key over two scopes is one
// cap; replacements move a file between dimensions; a purge releases.
func TestDimensionsCapAcrossScopes(t *testing.T) {
	e, exec := newEnv(t, fixed(quota.Limit{}), quota.WithDimensions(orgOf))
	a, err := e.add(t, "w1", "a", 40)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.add(t, "w2", "b", 40); err != nil {
		t.Fatal(err)
	}
	if _, err := e.add(t, "w2", "c", 30); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded || !strings.Contains(err.Error(), "org:acme") {
		t.Fatalf("over the org cap = %v", err)
	}
	if u := usage(t, e, "w2"); u.Bytes != 40 || u.Files != 1 {
		t.Fatalf("w2 = %+v: a refused file must charge nothing", u)
	}
	if u := dimUsage(t, e, "org:acme"); u.Bytes != 80 || u.Files != 2 {
		t.Fatalf("org = %+v", u)
	}

	// 40 -> 60 bytes: the org grows by 20; the file moves small -> big.
	if err := e.replaceWith(t, "w1", a.ID, 60); err != nil {
		t.Fatal(err)
	}
	if org, small, big := dimUsage(t, e, "org:acme"), dimUsage(t, e, "class:small"), dimUsage(t, e, "class:big"); org.Bytes != 100 || small.Bytes != 40 || small.Files != 1 || big.Bytes != 60 || big.Files != 1 {
		t.Fatalf("after replace: org %+v small %+v big %+v", org, small, big)
	}
	if err := e.replaceWith(t, "w1", a.ID, 61); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Fatalf("growing past the org cap = %v", err)
	}
	if err := e.store.Purge(t.Context(), "w1", a.ID); err != nil {
		t.Fatal(err)
	}
	if org, big := dimUsage(t, e, "org:acme"), dimUsage(t, e, "class:big"); org.Bytes != 40 || org.Files != 1 || big.Bytes != 0 || big.Files != 0 {
		t.Fatalf("after purge: org %+v big %+v", org, big)
	}

	// Drift repaired from the files.
	exec("UPDATE fs_quota_dimension_usage SET bytes = 999")
	if err := e.plugin.RecountDimensions(t.Context(), e.store); err != nil {
		t.Fatal(err)
	}
	if org := dimUsage(t, e, "org:acme"); org.Bytes != 40 || org.Files != 1 {
		t.Fatalf("after recount: org %+v", org)
	}
}

// TestDimensionsFailClosed: an error from the DimensionFunc, or a bad key,
// refuses the file before any counter moves.
func TestDimensionsFailClosed(t *testing.T) {
	answer := func(context.Context, records.File) ([]quota.Dimension, error) { return nil, errOrgs }
	e, _ := newEnv(t, fixed(quota.Limit{}), quota.WithDimensions(func(ctx context.Context, f records.File) ([]quota.Dimension, error) {
		return answer(ctx, f)
	}))
	if _, err := e.add(t, "w1", "a", 10); !errors.Is(err, errOrgs) {
		t.Fatalf("error = %v", err)
	}
	for name, dims := range map[string][]quota.Dimension{
		"empty":    {{Key: ""}},
		"repeated": {{Key: "x"}, {Key: "x"}},
		"long":     {{Key: strings.Repeat("k", quota.MaxDimensionBytes+1)}},
	} {
		answer = func(context.Context, records.File) ([]quota.Dimension, error) { return dims, nil }
		if _, err := e.add(t, "w1", "b-"+name, 10); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if u := usage(t, e, "w1"); u.Bytes != 0 || u.Files != 0 {
		t.Fatalf("scope charged %+v for refused files", u)
	}
}

// TestDimensionsDefaultOff: without WithDimensions nothing is charged to a
// dimension and RecountDimensions refuses.
func TestDimensionsDefaultOff(t *testing.T) {
	e, _ := newEnv(t, fixed(quota.Limit{Bytes: 100}))
	if _, err := e.add(t, "w1", "a", 10); err != nil {
		t.Fatal(err)
	}
	// No dimension row of any key: a default that charged some counter
	// (the scope's, a content class) would pass a check of one named key.
	var rows int64
	q := sqlb.NewSelect(e.store.Dialect()).Columns(sqlb.Bind(sqlb.Count(), &rows)).From(quota.DimensionTable)
	if err := e.store.One(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("%d dimension rows charged without the option", rows)
	}
	if err := e.plugin.RecountDimensions(t.Context(), e.store); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("RecountDimensions = %v", err)
	}
}

// sweepFaults runs op with each database step in turn failing until it
// completes, requiring every failed run to fail with the injected error.
func sweepFaults(t *testing.T, e *env, name string, op func() error) {
	t.Helper()
	for n := 1; ; n++ {
		e.faults.Arm(n)
		err := op()
		e.faults.Arm(0)
		if errors.Is(err, testdb.ErrInjected) {
			continue
		}
		if err != nil {
			t.Fatalf("%s: step %d: %v", name, n, err)
		}
		return
	}
}

// TestDimensionsFaults pins the dimension paths' failures: a database
// failure at any step of a charge, a replacement that grows, shrinks or
// moves a file to another counter, a purge or a recount fails the change
// with that error; a dimension function failing for the old or the new
// file refuses the replacement, and for a purged file refuses the purge.
func TestDimensionsFaults(t *testing.T) {
	ctx := t.Context()
	var failOn int // the dimension call that fails, counted from 1; 0 never
	calls := 0
	dims := func(ctx context.Context, f records.File) ([]quota.Dimension, error) {
		calls++
		if failOn != 0 && calls == failOn {
			return nil, errOrgs
		}
		return orgOf(ctx, f)
	}
	e, _ := newEnv(t, fixed(quota.Limit{Bytes: 1000}), quota.WithDimensions(dims))
	key := 0
	put := func(size int) (records.File, error) {
		key++
		return e.add(t, "w1", "f"+strings.Repeat("x", key), size)
	}
	var f records.File
	sweepFaults(t, e, "charge", func() error {
		var err error
		f, err = put(10)
		return err
	})
	sweepFaults(t, e, "grow into another class", func() error { return e.replaceWith(t, "w1", f.ID, 60) })
	sweepFaults(t, e, "shrink back", func() error { return e.replaceWith(t, "w1", f.ID, 20) })
	sweepFaults(t, e, "shrink within the class", func() error { return e.replaceWith(t, "w1", f.ID, 15) })
	sweepFaults(t, e, "purge", func() error { return e.store.Purge(ctx, "w1", f.ID) })
	g, err := put(10)
	if err != nil {
		t.Fatal(err)
	}
	sweepFaults(t, e, "recount", func() error { return e.plugin.RecountDimensions(ctx, e.store) })

	for name, at := range map[string]int{"old file's dimensions": 1, "new file's dimensions": 2} {
		calls, failOn = 0, at
		if err := e.replaceWith(t, "w1", g.ID, 30); !errors.Is(err, errOrgs) {
			t.Errorf("replace with the %s failing = %v", name, err)
		}
	}
	calls, failOn = 0, 1
	if err := e.store.Purge(ctx, "w1", g.ID); !errors.Is(err, errOrgs) {
		t.Errorf("purge with the dimensions failing = %v", err)
	}
	calls, failOn = 0, 1
	if err := e.plugin.RecountDimensions(ctx, e.store); !errors.Is(err, errOrgs) {
		t.Errorf("recount with the dimensions failing = %v", err)
	}
	failOn = 0
	if u := dimUsage(t, e, "org:acme"); u.Bytes != 10 || u.Files != 1 {
		t.Errorf("after the refused changes org:acme = %+v, want the one 10-byte file", u)
	}
}
