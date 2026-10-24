package records_test

import (
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// TestViewOnce: a view-once file is served exactly once; the first view
// sets its expiry and SweepExpired then purges it.
func TestViewOnce(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f, err := e.store.Upload(ctx, "t", "media", reader(pngData), "secret.png", images, records.ViewOnce())
	if err != nil {
		t.Fatal(err)
	}
	if !f.ViewOnce || f.ExpiresAt != nil {
		t.Fatalf("a new view-once file = %+v", f)
	}
	for i, want := range []bool{true, false, false} {
		ok, err := e.store.Viewed(ctx, "t", f.ID)
		if err != nil || ok != want {
			t.Fatalf("view %d = %v, %v; want %v", i+1, ok, err, want)
		}
	}
	read, _ := e.store.File(ctx, "t", f.ID)
	if read.ExpiresAt == nil || !read.Expired(e.now) {
		t.Fatalf("after the first view: %+v", read)
	}
	if n, err := e.store.SweepExpired(ctx, e.now); err != nil || n != 1 {
		t.Fatalf("SweepExpired = %d, %v", n, err)
	}
	if _, err := e.store.File(ctx, "t", f.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound || e.exists(t, f.Key) {
		t.Fatal("the expired file or its object outlived the sweep")
	}
	if _, err := e.store.Viewed(ctx, "t", f.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("Viewed of a purged file = %v", err)
	}
}

// TestExpiresAt: a file with an expiry is served until it, purged by the
// sweep after it, and a file without one is never touched.
func TestExpiresAt(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	at := e.now.Add(time.Hour)
	temp, err := e.store.Upload(ctx, "t", "media", reader(pngData), "story.png", images, records.Expires(at))
	if err != nil {
		t.Fatal(err)
	}
	keep := e.put(t, "t", "keep.png")
	if temp.ExpiresAt == nil || !temp.ExpiresAt.Equal(at) {
		t.Fatalf("ExpiresAt = %v", temp.ExpiresAt)
	}
	if ok, err := e.store.Viewed(ctx, "t", temp.ID); err != nil || !ok {
		t.Fatalf("before the expiry: %v, %v", ok, err)
	}
	if n, err := e.store.SweepExpired(ctx, at.Add(-time.Second)); err != nil || n != 0 {
		t.Fatalf("an early sweep purged %d, %v", n, err)
	}
	e.now = at // Viewed reads the Store's clock
	if ok, err := e.store.Viewed(ctx, "t", temp.ID); err != nil || ok {
		t.Fatalf("at the expiry: %v, %v", ok, err)
	}
	if n, err := e.store.SweepExpired(ctx, at); err != nil || n != 1 {
		t.Fatalf("SweepExpired = %d, %v", n, err)
	}
	if _, err := e.store.File(ctx, "t", keep.ID); err != nil {
		t.Fatalf("a file without expiry was swept: %v", err)
	}
	if ok, err := e.store.Viewed(ctx, "t", keep.ID); err != nil || !ok {
		t.Fatalf("an ordinary file: %v, %v", ok, err)
	}
}

// TestViewedTrashed: a trashed file is never served.
func TestViewedTrashed(t *testing.T) {
	e := newEnv(t, nil)
	f := e.put(t, "t", "a.png")
	if err := e.store.Trash(t.Context(), "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.store.Viewed(t.Context(), "t", f.ID); err != nil || ok {
		t.Fatalf("Viewed of a trashed file = %v, %v", ok, err)
	}
}
