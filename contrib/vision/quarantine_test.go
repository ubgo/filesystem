package vision_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ubgo/filesystem/contrib/vision"
	"github.com/ubgo/filesystem/records"
)

// TestPlugin_QuarantinedImage: a quarantined image is not sent to the
// labeler, on upload or on demand, and is labelled when its scan passes;
// other releases (a file not wanted) and other events cost nothing.
func TestPlugin_QuarantinedImage(t *testing.T) {
	l := &fakeLabeler{labels: []vision.Label{{Name: "Dog", Score: 0.98}}}
	var failures []error
	e, vp := newEnv(t, l, vision.WithErrorHandler(func(_ context.Context, _ records.File, err error) { failures = append(failures, err) }))
	ctx := t.Context()
	f, err := e.store.Upload(ctx, optedIn, "media", bytes.NewReader(pngHead), "dog.png", policy, records.Quarantine())
	if err != nil {
		t.Fatal(err)
	}
	if l.calls() != 0 || len(e.tagsOf(t, optedIn, f.ID)) != 0 || len(failures) != 0 {
		t.Fatalf("a quarantined image was labelled: %d calls, failures %v", l.calls(), failures)
	}
	if _, err := vp.Label(ctx, e.store, optedIn, f.ID); !errors.Is(err, records.ErrQuarantined) || l.calls() != 0 {
		t.Fatalf("Label of a quarantined image = %v, %d calls", err, l.calls())
	}
	if _, err := e.store.SetStatus(ctx, optedIn, f.ID, records.StatusQuarantined, "queued"); err != nil || l.calls() != 0 {
		t.Fatalf("a status change labelled: %v, %d calls", err, l.calls())
	}
	if _, err := e.store.PassScan(ctx, optedIn, f.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.tagsOf(t, optedIn, f.ID); l.calls() != 1 || !slices.Equal(got, []string{"dog"}) {
		t.Fatalf("after release: %d calls, tags %q", l.calls(), got)
	}
	g, err := e.store.Upload(ctx, other, "media", bytes.NewReader(pngHead), "dog.png", policy, records.Quarantine())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.PassScan(ctx, other, g.ID); err != nil || l.calls() != 1 {
		t.Fatalf("release in a scope not opted in: %v, %d calls", err, l.calls())
	}
}
