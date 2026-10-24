package quota_test

import (
	"context"
	"sync"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/quota"
)

// orgDirectory maps workspaces to organisations and can move one: the
// application's directory a DimensionFunc reads.
type orgDirectory struct {
	mu  sync.Mutex
	org map[string]string
}

func (d *orgDirectory) dims(_ context.Context, f records.File) ([]quota.Dimension, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return []quota.Dimension{{Key: "org:" + d.org[f.Scope]}, {Key: "ws:" + f.Scope}}, nil
}

func (d *orgDirectory) move(scope, org string) { d.mu.Lock(); d.org[scope] = org; d.mu.Unlock() }

// TestRecountDimensionsFor: after a workspace moves to another
// organisation, a recount of the two organisation keys over their
// workspaces moves its bytes, leaves every other row alone, and reads only
// the listed scopes.
func TestRecountDimensionsFor(t *testing.T) {
	dir := &orgDirectory{org: map[string]string{"w1": "acme", "w2": "acme", "w3": "beta"}}
	e, exec := newEnv(t, fixed(quota.Limit{}), quota.WithDimensions(dir.dims))
	for _, f := range []struct {
		scope, name string
		size        int
	}{{"w1", "a", 10}, {"w2", "b", 20}, {"w3", "c", 30}} {
		if _, err := e.add(t, f.scope, f.name, f.size); err != nil {
			t.Fatal(err)
		}
	}
	dir.move("w2", "beta")
	exec("UPDATE fs_quota_dimension_usage SET bytes = 999 WHERE dimension LIKE 'ws:%'")
	ctx := t.Context()
	if err := e.plugin.RecountDimensionsFor(ctx, e.store, quota.RecountFilter{Keys: []string{"org:acme", "org:beta"}, Scopes: []string{"w1", "w2", "w3"}}); err != nil {
		t.Fatal(err)
	}
	if acme, beta := dimUsage(t, e, "org:acme"), dimUsage(t, e, "org:beta"); acme.Bytes != 10 || acme.Files != 1 || beta.Bytes != 50 || beta.Files != 2 {
		t.Fatalf("after the move: acme %+v beta %+v", acme, beta)
	}
	if ws := dimUsage(t, e, "ws:w2"); ws.Bytes != 999 {
		t.Fatalf("an unlisted key was rebuilt: %+v", ws)
	}
	// Only the listed scopes are read: beta rebuilt from w3 alone.
	if err := e.plugin.RecountDimensionsFor(ctx, e.store, quota.RecountFilter{Keys: []string{"org:beta"}, Scopes: []string{"w3"}}); err != nil {
		t.Fatal(err)
	}
	if beta := dimUsage(t, e, "org:beta"); beta.Bytes != 30 || beta.Files != 1 {
		t.Fatalf("beta from w3 = %+v", beta)
	}
	// A listed key no file maps to any more is removed; Keys alone reads
	// every scope.
	dir.move("w1", "beta")
	if err := e.plugin.RecountDimensionsFor(ctx, e.store, quota.RecountFilter{Keys: []string{"org:acme", "org:beta"}}); err != nil {
		t.Fatal(err)
	}
	if acme, beta := dimUsage(t, e, "org:acme"), dimUsage(t, e, "org:beta"); acme.Bytes != 0 || acme.Files != 0 || beta.Bytes != 60 || beta.Files != 3 {
		t.Fatalf("after the second move: acme %+v beta %+v", acme, beta)
	}
	if err := e.plugin.RecountDimensionsFor(ctx, e.store, quota.RecountFilter{Scopes: []string{"w1"}}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("Scopes without Keys = %v", err)
	}
	// The zero filter is the full recount.
	if err := e.plugin.RecountDimensionsFor(ctx, e.store, quota.RecountFilter{}); err != nil {
		t.Fatal(err)
	}
	if ws := dimUsage(t, e, "ws:w2"); ws.Bytes != 20 {
		t.Fatalf("full recount: ws:w2 = %+v", ws)
	}
}
