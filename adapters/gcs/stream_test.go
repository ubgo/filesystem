package gcs_test

import (
	"errors"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gcs"
)

// failAfter yields n bytes, then fails.
type failAfter struct{ n int }

var errBody = errors.New("body broke")

func (f *failAfter) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errBody
	}
	k := min(len(p), f.n)
	for i := range k {
		p[i] = 'x'
	}
	f.n -= k
	return k, nil
}

// minChunk is GCS's smallest resumable chunk.
const minChunk = 256 << 10

func TestLargeFailingBodyIsNotCommitted(t *testing.T) {
	ctx := t.Context()
	srv := fakeServer(t)
	drv, _ := gcs.New(gcs.Config{Client: srv.Client(), Bucket: newBucket(t, srv), ChunkSize: minChunk})
	d, _ := filesystem.NewDisk("gcs", drv)
	if _, err := d.Write(ctx, "big", &failAfter{n: 600 << 10}, filesystem.WriteOptions{}); err == nil {
		t.Fatal("write with a failing body succeeded")
	}
	if _, err := d.Stat(ctx, "big"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("a partial object was committed: %v", err)
	}
	// A large body that succeeds streams through the resumable path.
	big := make([]byte, 700<<10)
	if _, err := d.WriteBytes(ctx, "ok", big, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if e, _ := d.Stat(ctx, "ok"); e.Size != int64(len(big)) {
		t.Errorf("size %d", e.Size)
	}
}
