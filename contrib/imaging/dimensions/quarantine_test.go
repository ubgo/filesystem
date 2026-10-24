package dimensions_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging/dimensions"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

// readCounter is a disk interceptor counting object reads.
type readCounter struct{ reads int }

func (c *readCounter) intercept(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
	if op.Kind == filesystem.OpRead {
		c.reads++
	}
	return call(ctx)
}

// TestQuarantinedImage: a quarantined image is recorded without reading
// its unscanned bytes (and without dimensions a caller tried to set),
// Backfill leaves it alone, and the scan passing measures it; a
// replacement quarantined again drops the old dimensions until its own
// release.
func TestQuarantinedImage(t *testing.T) {
	ctx := t.Context()
	c := &readCounter{}
	disk, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(c.intercept))
	e := newEnvOn(t, disk)
	f, err := e.record(t, "q.png", pngOf(t, 30, 20), records.NewFile{ContentType: "image/png", Status: records.StatusQuarantined, Metadata: map[string]string{dimensions.MetaWidth: "9999"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := dimensions.Of(f); ok || f.Metadata[dimensions.MetaWidth] != "" || c.reads != 0 {
		t.Fatalf("a quarantined image was measured: %v, %d reads", f.Metadata, c.reads)
	}
	res, err := dimensions.Backfill(ctx, e.store, dimensions.BackfillOptions{Scope: "t"})
	if err != nil || res.Measured != 0 || c.reads != 0 {
		t.Fatalf("Backfill over a quarantined image = %+v, %v, %d reads", res, err, c.reads)
	}
	got, err := e.store.PassScan(ctx, "t", f.ID)
	if err != nil || dims(got) != [3]int{30, 20, 1} || c.reads == 0 {
		t.Fatalf("after release: %v, %v, %d reads", dims(got), err, c.reads)
	}
	if read, _ := e.store.File(ctx, "t", f.ID); dims(read) != [3]int{30, 20, 1} {
		t.Fatalf("stored after release: %v", dims(read))
	}

	images := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}}
	before := c.reads
	got, err = e.store.Replace(ctx, "t", f.ID, bytes.NewReader(pngOf(t, 8, 6)), "", images, records.QuarantineContent())
	if err != nil || dims(got) != [3]int{0, 0, 0} {
		t.Fatalf("quarantined replacement: %v, %v", dims(got), err)
	}
	if c.reads != before {
		t.Fatal("the quarantined replacement was read")
	}
	if got, err := e.store.PassScan(ctx, "t", f.ID); err != nil || dims(got) != [3]int{8, 6, 1} {
		t.Fatalf("after the second release: %v, %v", dims(got), err)
	}
}
