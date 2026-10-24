package main

import (
	"context"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

func disks(t *testing.T) (src, dst *filesystem.Disk) {
	t.Helper()
	src, err := seedSource(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dst, stop, err := fakeTarget(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return src, dst
}

func TestCopyResumesAndVerifies(t *testing.T) {
	ctx := t.Context()
	src, dst := disks(t)
	dry, err := Copy(ctx, src, dst, Options{Workers: 3, DryRun: true})
	if err != nil || dry.Copied != int64(len(demoFiles)) {
		t.Fatalf("dry run = %+v, %v", dry, err)
	}
	if ok, _ := dst.Exists(ctx, "avatars/u1.png"); ok {
		t.Fatal("dry run copied")
	}
	r, err := Copy(ctx, src, dst, Options{Workers: 3})
	if err != nil || r.Copied != int64(len(demoFiles)) || len(r.Failed) != 0 {
		t.Fatalf("copy = %+v, %v", r, err)
	}
	e, err := dst.Stat(ctx, "invoices/2025/0001.pdf")
	if err != nil || e.ContentType != "application/pdf" {
		t.Errorf("content type on target = %q, %v", e.ContentType, err)
	}
	again, err := Copy(ctx, src, dst, Options{Workers: 3})
	if err != nil || again.Copied != 0 || again.Skipped != int64(len(demoFiles)) {
		t.Errorf("second run = %+v, %v", again, err)
	}
	if bad, err := Verify(ctx, src, dst); err != nil || len(bad) != 0 {
		t.Errorf("verify = %v, %v", bad, err)
	}
	// A changed size is copied again; same-size corruption is caught by Verify.
	_, _ = src.WriteBytes(ctx, "avatars/u1.png", []byte(demoFiles["avatars/u1.png"]+"-v2"), filesystem.WriteOptions{})
	_, _ = dst.WriteBytes(ctx, "avatars/u2.png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-XX"), filesystem.WriteOptions{})
	if bad, _ := Verify(ctx, src, dst); !slices.Equal(bad, []string{"avatars/u1.png", "avatars/u2.png"}) {
		t.Errorf("verify after changes = %v", bad)
	}
	if r, _ := Copy(ctx, src, dst, Options{}); r.Copied != 1 {
		t.Errorf("copy after change = %+v", r)
	}
}

// failingReads makes reads of one key fail, as a flaky source would.
func failingReads(key string) filesystem.Interceptor {
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpRead && op.Key == key {
			return filesystem.Errorf(filesystem.CodeUnavailable, "injected")
		}
		return call(ctx)
	}
}

func TestFailuresAreRetriedByRerun(t *testing.T) {
	ctx := t.Context()
	good, dst := disks(t)
	flaky, err := filesystem.NewDisk(diskName, memory.New(), filesystem.WithInterceptors(failingReads("invoices/2025/0002.pdf")))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range slices.Sorted(maps.Keys(demoFiles)) {
		if _, err := filesystem.CopyAcross(ctx, good, k, flaky, k); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Copy(ctx, flaky, dst, Options{Workers: 2})
	if err != nil || len(r.Failed) != 1 || r.Copied != int64(len(demoFiles)-1) {
		t.Fatalf("copy = %+v, %v", r, err)
	}
	if _, ok := r.Failed["invoices/2025/0002.pdf"]; !ok {
		t.Errorf("failed = %v", r.Failed)
	}
	r, _ = Copy(ctx, good, dst, Options{})
	if r.Copied != 1 || r.Skipped != int64(len(demoFiles)-1) {
		t.Errorf("rerun = %+v", r)
	}
	rc, err := dst.Read(ctx, "invoices/2025/0002.pdf")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != demoFiles["invoices/2025/0002.pdf"] {
		t.Errorf("retried object = %q", b)
	}
}

func TestDemoAndEnv(t *testing.T) {
	if err := demo(t.Context(), Options{Workers: 2}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envSource, "file:///tmp")
	t.Setenv(envTarget, "")
	if _, _, ok, err := openFromEnv(t.Context()); !ok || err == nil {
		t.Errorf("half-set env = %v %v", ok, err)
	}
	t.Setenv(envTarget, "file://"+t.TempDir())
	t.Setenv(envSource, "file://"+t.TempDir())
	if src, _, _, err := openFromEnv(t.Context()); err != nil {
		t.Fatal(err)
	} else if _, err := src.WriteBytes(t.Context(), "x", []byte("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeReadOnly {
		t.Errorf("source is writable: %v", err)
	}
}

// TestDemoDryRun runs the demo as `-dry-run` does: the two copies still run
// for real, so the resume step has nothing to do and the dry run finds
// only the new upload.
func TestDemoDryRun(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	runErr := demo(t.Context(), Options{Workers: 2, DryRun: true})
	os.Stdout = stdout
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if runErr != nil {
		t.Fatal(runErr)
	}
	for _, want := range []string{"copied 6, skipped 0", "copied 0, skipped 6", "to copy 1, skipped 6"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("demo output lacks %q:\n%s", want, out)
		}
	}
}
