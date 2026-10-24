package filesystem_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// batchDriver is the memory driver with a scripted DeleteKeys: refused
// keys come back in a BatchError, and whole fails the call outright.
type batchDriver struct {
	*memory.Driver
	refuse map[string]bool
	whole  error
	native bool // false hides the DeleteKeys capability
}

func (b *batchDriver) Capabilities() filesystem.Capabilities {
	c := b.Driver.Capabilities()
	c.DeleteKeys = b.native
	return c
}

func (b *batchDriver) Delete(ctx context.Context, key string) error {
	if b.refuse[key] {
		return filesystem.ErrPermission
	}
	return b.Driver.Delete(ctx, key)
}

func (b *batchDriver) DeleteKeys(ctx context.Context, keys []string) error {
	if b.whole != nil {
		return b.whole
	}
	var failed []filesystem.KeyError
	var ok []string
	for _, k := range keys {
		if b.refuse[k] {
			// A classified *Error, as real drivers return: annotate must
			// not mistake it for the error of the whole batch.
			failed = append(failed, filesystem.KeyError{Key: k, Err: filesystem.Errorf(filesystem.CodePermission, "locked by policy")})
		} else {
			ok = append(ok, k)
		}
	}
	_ = b.Driver.DeleteKeys(ctx, ok)
	if failed != nil {
		return &filesystem.BatchError{Failures: failed}
	}
	return nil
}

func seeded(t *testing.T, drv *batchDriver, keys ...string) *filesystem.Disk {
	t.Helper()
	d, err := filesystem.NewDisk("b", drv)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if _, err := d.WriteBytes(t.Context(), k, []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func remaining(t *testing.T, d *filesystem.Disk) string {
	t.Helper()
	page, err := d.ListPage(t.Context(), "", filesystem.ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, e := range page.Entries {
		keys = append(keys, e.Key)
	}
	return strings.Join(keys, ",")
}

func TestDeleteKeysPartialFailure(t *testing.T) {
	for _, native := range []bool{true, false} {
		drv := &batchDriver{Driver: memory.New(), refuse: map[string]bool{"locked": true}, native: native}
		d := seeded(t, drv, "a", "b", "locked")
		var (
			ops []filesystem.OpKind
			mu  sync.Mutex
		)
		// The fallback deletes keys in parallel, so the recorder locks.
		recorder, _ := filesystem.NewDisk("b", drv, filesystem.WithInterceptors(func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
			mu.Lock()
			ops = append(ops, op.Kind)
			mu.Unlock()
			return call(ctx)
		}))
		err := recorder.DeleteKeys(t.Context(), []string{"a", "locked", "b"})
		var batch *filesystem.BatchError
		if !errors.As(err, &batch) || len(batch.Failures) != 1 || batch.Failures[0].Key != "locked" {
			t.Fatalf("native %v: err = %v", native, err)
		}
		if !errors.Is(err, filesystem.ErrPermission) || filesystem.CodeOf(err) != filesystem.CodePermission || !strings.Contains(err.Error(), "1 keys failed; locked") {
			t.Fatalf("native %v: classification of %v", native, err)
		}
		if got := remaining(t, d); got != "locked" {
			t.Fatalf("native %v: left %s", native, got)
		}
		want := "delete_keys"
		if !native {
			want = "delete,delete,delete"
		}
		var kinds []string
		for _, k := range ops {
			kinds = append(kinds, string(k))
		}
		if strings.Join(kinds, ",") != want {
			t.Fatalf("native %v: ops %v", native, kinds)
		}
	}
}

func TestDeleteKeysWholeFailure(t *testing.T) {
	down := errors.New("connection reset")
	drv := &batchDriver{Driver: memory.New(), whole: down, native: true}
	d := seeded(t, drv, "a", "b")
	err := d.DeleteKeys(t.Context(), []string{"a", "b"})
	var batch *filesystem.BatchError
	if !errors.As(err, &batch) || batch.Failures[0].Err == nil {
		t.Fatalf("err = %v", err)
	}
	if !errors.As(err, &batch) || len(batch.Failures) != 2 || !errors.Is(err, down) {
		t.Fatalf("err = %v", err)
	}
	if got := remaining(t, d); got != "a,b" {
		t.Fatalf("left %s", got)
	}
}

func TestDeleteKeysThroughWrappers(t *testing.T) {
	ctx := t.Context()
	drv := &batchDriver{Driver: memory.New(), refuse: map[string]bool{"tenant/locked": true}, native: true}
	root := seeded(t, drv, "tenant/a", "tenant/locked", "other/a")
	scoped, err := root.Scope("tenant", "tenant")
	if err != nil {
		t.Fatal(err)
	}
	err = scoped.DeleteKeys(ctx, []string{"a", "locked"})
	var batch *filesystem.BatchError
	if !errors.As(err, &batch) || batch.Failures[0].Key != "locked" {
		t.Fatalf("scoped failures = %v", err)
	}
	if err := scoped.DeleteKeys(ctx, []string{"gone"}); err != nil {
		t.Fatalf("scoped success = %v", err)
	}
	if got := remaining(t, root); got != "other/a,tenant/locked" {
		t.Fatalf("left %s", got)
	}

	copyDisk, _ := filesystem.NewDisk("copy", memory.New())
	_, _ = copyDisk.WriteBytes(ctx, "m", []byte("x"), filesystem.WriteOptions{})
	primary := seeded(t, &batchDriver{Driver: memory.New(), native: true}, "m")
	mirrored, err := filesystem.Mirror("mirrored", primary, copyDisk)
	if err != nil {
		t.Fatal(err)
	}
	if err := mirrored.DeleteKeys(ctx, []string{"m"}); err != nil {
		t.Fatal(err)
	}
	if remaining(t, primary) != "" || remaining(t, copyDisk) != "" {
		t.Fatal("the mirror kept a key")
	}
	failing := seeded(t, &batchDriver{Driver: memory.New(), native: true, whole: filesystem.ErrUnavailable}, "m")
	broken, _ := filesystem.Mirror("broken", failing, copyDisk)
	if err := broken.DeleteKeys(ctx, []string{"m"}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("primary failure = %v", err)
	}
}
