package filesystem_test

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// leakyConditional answers ReadIf with a reader and an error together, as
// some SDKs do on a failed response.
type leakyConditional struct {
	*memory.Driver
	closed atomic.Int64
}

func (l *leakyConditional) Capabilities() filesystem.Capabilities {
	c := l.Driver.Capabilities()
	c.ConditionalRead = true
	return c
}

func (l *leakyConditional) ReadIf(context.Context, string, filesystem.ReadOptions) (io.ReadCloser, filesystem.Entry, error) {
	return &countClose{ReadCloser: io.NopCloser(strings.NewReader("partial")), n: &l.closed}, filesystem.Entry{}, filesystem.ErrUnavailable
}

type countClose struct {
	io.ReadCloser
	n *atomic.Int64
}

func (c *countClose) Close() error { c.n.Add(1); return c.ReadCloser.Close() }

func TestReadIfClosesTheReaderOfAFailedCall(t *testing.T) {
	drv := &leakyConditional{Driver: memory.New()}
	d := mustDisk(t, "d", drv)
	if _, _, err := d.ReadIf(t.Context(), "k", filesystem.ReadOptions{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("ReadIf: %v", err)
	}
	if drv.closed.Load() != 1 {
		t.Errorf("the reader returned with the error was closed %d times", drv.closed.Load())
	}
}

// countingOpens is a memory driver whose readers count their closes.
type countingOpens struct {
	*memory.Driver
	opened, closed atomic.Int64
}

func (c *countingOpens) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	rc, err := c.Driver.Read(ctx, key, rng)
	if err != nil {
		return nil, err
	}
	c.opened.Add(1)
	return &countClose{ReadCloser: rc, n: &c.closed}, nil
}

// TestSequentialRecallsReplaceTheResult pins that an interceptor calling
// the operation again after a success (not concurrently, as Hedge does)
// gets the later result, with the earlier reader closed, and never
// ErrSuperseded.
func TestSequentialRecallsReplaceTheResult(t *testing.T) {
	ctx := t.Context()
	drv := &countingOpens{Driver: memory.New()}
	twice := func(ctx context.Context, _ *filesystem.Op, call filesystem.Call) error {
		if err := call(ctx); err != nil {
			return err
		}
		return call(ctx)
	}
	d := mustDisk(t, "d", drv, filesystem.WithInterceptors(twice))
	if _, err := d.WriteBytes(ctx, "k", []byte("hello"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Stat(ctx, "k"); err != nil {
		t.Fatalf("stat called twice: %v", err)
	}
	rc, err := d.Read(ctx, "k")
	if err != nil {
		t.Fatalf("read called twice: %v", err)
	}
	if drv.opened.Load() != 2 || drv.closed.Load() != 1 {
		t.Errorf("after two opens, %d opened and %d closed; want the first closed", drv.opened.Load(), drv.closed.Load())
	}
	if b, _ := io.ReadAll(rc); string(b) != "hello" {
		t.Errorf("read %q", b)
	}
	_ = rc.Close()
	if drv.closed.Load() != 2 {
		t.Errorf("%d readers closed after the caller closed its own", drv.closed.Load())
	}
	if filesystem.CodeOf(filesystem.ErrSuperseded) != filesystem.CodeConflict {
		t.Errorf("ErrSuperseded has code %q", filesystem.CodeOf(filesystem.ErrSuperseded))
	}
}
