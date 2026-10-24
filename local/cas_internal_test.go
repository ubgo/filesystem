package local

import (
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
)

// slowStat delays every Stat of key, widening the window between the
// ETag check and the rename that the key's lock must close.
type slowStat struct {
	fileRoot
	key string
}

func (s slowStat) Stat(name string) (fs.FileInfo, error) {
	if name == s.key {
		time.Sleep(20 * time.Millisecond)
	}
	return s.fileRoot.Stat(name)
}

func TestCompareAndSwapIsSerialized(t *testing.T) {
	ctx := t.Context()
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.Write(ctx, "doc.json", strings.NewReader("v1"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	e, err := d.Stat(ctx, "doc.json")
	if err != nil {
		t.Fatal(err)
	}
	d.root = slowStat{d.root, "doc.json"}
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, body := range []string{"from-a", "from-b-longer"} {
		wg.Go(func() {
			_, results[i] = d.Write(ctx, "doc.json", strings.NewReader(body), filesystem.WriteOptions{IfMatch: e.ETag})
		})
	}
	wg.Wait()
	won := 0
	for _, err := range results {
		if err == nil {
			won++
		} else if filesystem.CodeOf(err) != filesystem.CodeConflict {
			t.Errorf("racing swap: %v", err)
		}
	}
	if won != 1 {
		t.Errorf("%d racing swaps won, want exactly one", won)
	}
}
