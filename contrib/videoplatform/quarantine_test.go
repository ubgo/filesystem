package videoplatform_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ubgo/filesystem/records"
)

// TestPublish_Quarantined: a quarantined video is not handed to the
// platform, even with a WithSourceURL that would sign anything; after its
// scan passes it publishes.
func TestPublish_Quarantined(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	f, err := e.store.Upload(ctx, scope, "media", bytes.NewReader(mp4), "clip.mp4", policy, records.Quarantine())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.plugin.Publish(ctx, e.store, scope, f.ID); !errors.Is(err, records.ErrQuarantined) || len(e.fake.sources) != 0 {
		t.Fatalf("Publish of a quarantined video = %v, %d ingests", err, len(e.fake.sources))
	}
	if _, err := e.store.PassScan(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.plugin.Publish(ctx, e.store, scope, f.ID); err != nil || len(e.fake.sources) != 1 {
		t.Fatalf("Publish after release = %v, %d ingests", err, len(e.fake.sources))
	}
}
