package filesystem_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

func TestBatchBoundsParallelismAndCollectsFailures(t *testing.T) {
	var running, peak atomic.Int64
	keys := []string{"a", "b", "bad", "c", "d", "e", "f", "g"}
	err := filesystem.Batch(t.Context(), keys, 3, func(_ context.Context, k string) error {
		n := running.Add(1)
		defer running.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		if k == "bad" {
			return filesystem.ErrPermission
		}
		return nil
	})
	if p := peak.Load(); p != 3 {
		t.Errorf("peak parallelism %d, want 3", p)
	}
	var batch *filesystem.BatchError
	if !errors.As(err, &batch) || len(batch.Failures) != 1 || batch.Failures[0].Key != "bad" {
		t.Fatalf("want one failure for bad, got %v", err)
	}
	if err := filesystem.Batch(t.Context(), keys, 0, func(context.Context, string) error { return nil }); err != nil {
		t.Errorf("all succeeded: %v", err)
	}
}

func TestBatchStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var ran atomic.Int64
	err := filesystem.Batch(ctx, []string{"a", "b", "c", "d"}, 1, func(context.Context, string) error {
		ran.Add(1)
		cancel()
		return nil
	})
	var batch *filesystem.BatchError
	if !errors.As(err, &batch) || len(batch.Failures) != 3 || ran.Load() != 1 {
		t.Fatalf("ran %d, err %v", ran.Load(), err)
	}
	if filesystem.CodeOf(batch.Failures[0].Err) != filesystem.CodeUnavailable {
		t.Errorf("unstarted key failed with %v", batch.Failures[0].Err)
	}
}

// slowDeletes is a memory driver without bulk deletes whose Delete takes a
// while, to show the Disk's fallbacks delete in parallel.
type slowDeletes struct{ *memory.Driver }

func (s *slowDeletes) Capabilities() filesystem.Capabilities {
	c := s.Driver.Capabilities()
	c.DeleteKeys, c.DeletePrefix = false, false
	return c
}

func (s *slowDeletes) Delete(ctx context.Context, key string) error {
	time.Sleep(20 * time.Millisecond)
	return s.Driver.Delete(ctx, key)
}

func TestDeleteFallbacksRunInParallel(t *testing.T) {
	ctx := t.Context()
	d := mustDisk(t, "d", &slowDeletes{Driver: memory.New()})
	var keys []string
	for i := range 16 {
		k := "dir/" + string(rune('a'+i))
		keys = append(keys, k)
		if _, err := d.WriteBytes(ctx, k, []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// Sixteen 20ms deletes one by one take 320ms; in parallel, well under.
	start := time.Now()
	if err := d.DeletePrefix(ctx, "dir"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Errorf("DeletePrefix fallback took %s", took)
	}
	if got := allKeys(t, d, ""); len(got) != 0 {
		t.Errorf("left %v", got)
	}
	for _, k := range keys {
		_, _ = d.WriteBytes(ctx, k, []byte("x"), filesystem.WriteOptions{})
	}
	start = time.Now()
	if err := d.DeleteKeys(ctx, keys); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Errorf("DeleteKeys fallback took %s", took)
	}
}
