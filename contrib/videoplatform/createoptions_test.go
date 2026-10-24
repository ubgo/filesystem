package videoplatform_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/videoplatform"
	"github.com/ubgo/filesystem/records"
)

// TestWithCreateOptions pins the per-file create-options seam (R-I21):
// fn sees the file being published and its fields reach the platform's
// Source; an error from fn fails Publish before the platform is called,
// leaving no asset and no row; and without the option every Source
// carries no options.
func TestWithCreateOptions(t *testing.T) {
	var seen string
	e := newEnv(t, videoplatform.WithCreateOptions(func(_ context.Context, _ *records.Store, f records.File) (map[string]json.RawMessage, error) {
		seen = f.ID
		return map[string]json.RawMessage{"max_resolution_tier": json.RawMessage(`"1080p"`)}, nil
	}))
	f := e.video(t)
	if _, err := e.plugin.Publish(t.Context(), e.store, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if seen != f.ID || len(e.fake.sources) != 1 || string(e.fake.sources[0].Options["max_resolution_tier"]) != `"1080p"` {
		t.Fatalf("fn saw %q, sources %+v", seen, e.fake.sources)
	}

	boom := errors.New("no plan for this tenant")
	failing := newEnv(t, videoplatform.WithCreateOptions(func(context.Context, *records.Store, records.File) (map[string]json.RawMessage, error) {
		return nil, boom
	}))
	ff := failing.video(t)
	if _, err := failing.plugin.Publish(t.Context(), failing.store, scope, ff.ID); !errors.Is(err, boom) {
		t.Fatalf("a create-options error: %v", err)
	}
	if len(failing.fake.sources) != 0 {
		t.Fatalf("the platform was called after a create-options error: %+v", failing.fake.sources)
	}
	if _, err := failing.plugin.Asset(t.Context(), failing.store, scope, ff.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a failed Publish left a row: %v", err)
	}

	plain := newEnv(t)
	if _, err := plain.plugin.Publish(t.Context(), plain.store, scope, plain.video(t).ID); err != nil {
		t.Fatal(err)
	}
	if plain.fake.sources[0].Options != nil {
		t.Fatalf("without WithCreateOptions the Source carried %v", plain.fake.sources[0].Options)
	}
}
