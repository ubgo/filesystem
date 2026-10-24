package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

func TestRun(t *testing.T) {
	t.Setenv(envMirrorDSN, "") // restored after the test
	if err := os.Unsetenv(envMirrorDSN); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"mirror: a new write reached media-local=true media-s3=true",
		"backfill: copied 5, skipped 1",
		"backfill again: copied 0, skipped 6",
		"ok=true",
		"signed PUT on the mirror: unsupported",
		"backup: full run copied 7",
		"backup: incremental run copied 1",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func memDisk(t *testing.T, name string, opts ...filesystem.Option) *filesystem.Disk {
	t.Helper()
	d, err := filesystem.NewDisk(name, memory.New(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestVerifyFindsEveryDifference(t *testing.T) {
	ctx := t.Context()
	src, dst := memDisk(t, "src"), memDisk(t, "dst")
	_, _ = src.WriteBytes(ctx, "same", []byte("a"), filesystem.WriteOptions{})
	_, _ = dst.WriteBytes(ctx, "same", []byte("a"), filesystem.WriteOptions{})
	_, _ = src.WriteBytes(ctx, "changed", []byte("new"), filesystem.WriteOptions{})
	_, _ = dst.WriteBytes(ctx, "changed", []byte("old"), filesystem.WriteOptions{})
	_, _ = src.WriteBytes(ctx, "missing", []byte("m"), filesystem.WriteOptions{})
	_, _ = dst.WriteBytes(ctx, "extra", []byte("x"), filesystem.WriteOptions{})
	r, err := verify(ctx, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if r.OK() || len(r.Different) != 1 || len(r.Missing) != 1 || len(r.Extra) != 1 {
		t.Errorf("report = %+v", r)
	}
	// Same size, other content: backfill skips it, verify catches it.
	if copied, _, _ := backfill(ctx, src, dst); copied != 1 {
		t.Errorf("backfill copied %d, want only the missing one", copied)
	}
}

func TestMirrorFailureIsReturnedAndRetryConverges(t *testing.T) {
	ctx := t.Context()
	down := true
	failing := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if down && op.Mutating {
			return filesystem.Errorf(filesystem.CodeUnavailable, "mirror down")
		}
		return call(ctx)
	}
	primary, replica := memDisk(t, "p"), memDisk(t, "r", filesystem.WithInterceptors(failing))
	app, err := filesystem.Mirror(appDisk, primary, replica)
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.WriteBytes(ctx, "k", []byte("v"), filesystem.WriteOptions{})
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("write with a failing mirror = %v", err)
	}
	if ok, _ := primary.Exists(ctx, "k"); !ok {
		t.Error("primary lacks the object")
	}
	down = false
	if _, err := app.WriteBytes(ctx, "k", []byte("v"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := replica.Exists(ctx, "k"); !ok {
		t.Error("retry did not converge")
	}
}
