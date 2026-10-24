package s3_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// TestFindAbandoned proves the preview lists the uploads a sweep would
// abort, with their keys, aborts nothing, is declared only with the
// sweep, and agrees with the sweep that follows it.
func TestFindAbandoned(t *testing.T) {
	ctx := t.Context()
	if off, _ := newDisk(t, fss3.Config{}); off.Capabilities().FindAbandoned {
		t.Error("FindAbandoned declared without SweepUploads")
	}
	d, _ := newDisk(t, fss3.Config{SweepUploads: true})
	a, _ := d.CreateMultipart(ctx, "a.bin", filesystem.WriteOptions{})
	b, _ := d.CreateMultipart(ctx, "dir/b.bin", filesystem.WriteOptions{})
	if found, err := d.FindAbandoned(ctx, time.Now().Add(-time.Hour)); err != nil || len(found) != 0 {
		t.Fatalf("preview with an old cutoff = %+v, %v", found, err)
	}
	cutoff := time.Now().Add(time.Hour)
	found, err := d.FindAbandoned(ctx, cutoff)
	if err != nil || len(found) != 2 {
		t.Fatalf("preview = %+v, %v; want 2", found, err)
	}
	want := map[string]string{a: "a.bin", b: "dir/b.bin"}
	for _, u := range found {
		if u.Kind != filesystem.AbandonedMultipart || want[u.ID] != u.Key || u.LastActivity.IsZero() || !u.LastActivity.Before(cutoff) {
			t.Errorf("reported %+v", u)
		}
	}
	if _, err := d.ListParts(ctx, "a.bin", a); err != nil {
		t.Fatalf("the preview aborted an upload: %v", err)
	}
	if n, err := d.SweepAbandoned(ctx, cutoff); err != nil || n != len(found) {
		t.Fatalf("sweep after the preview = %d, %v; want %d", n, err, len(found))
	}
	if found, err := d.FindAbandoned(ctx, cutoff); err != nil || slices.ContainsFunc(found, func(u filesystem.Abandoned) bool { return u.ID == a || u.ID == b }) {
		t.Errorf("preview after the sweep = %+v, %v", found, err)
	}
}

// TestFindAbandonedListFailure: a failed listing is the preview's error.
func TestFindAbandonedListFailure(t *testing.T) {
	ctx := t.Context()
	d, p := newDisk(t, fss3.Config{SweepUploads: true})
	if _, err := d.CreateMultipart(ctx, "x.bin", filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	p.fail(http.MethodGet, "uploads", http.StatusInternalServerError, "InternalError")
	if found, err := d.FindAbandoned(ctx, time.Now().Add(time.Hour)); err == nil || found != nil {
		t.Errorf("preview = %+v, %v; want an error", found, err)
	}
}
