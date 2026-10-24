package filesystem_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// failWhen is an interceptor failing the operations match picks with
// errBroken.
func failWhen(match func(op *filesystem.Op) bool) filesystem.Interceptor {
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if match(op) {
			return errBroken
		}
		return call(ctx)
	}
}

// keysOfPage returns the keys of a page.
func keysOfPage(p filesystem.Page) []string {
	out := make([]string, len(p.Entries))
	for i, e := range p.Entries {
		out[i] = e.Key
	}
	return out
}

// TestRouteKeysOutsideEveryMount proves a routed disk without a catch-all
// answers keys under no mount as missing for reads, refuses them as a copy
// or move target, lists nothing there, and skips them in DeleteKeys.
func TestRouteKeysOutsideEveryMount(t *testing.T) {
	ctx := t.Context()
	a := mustDisk(t, "a", memory.New())
	r, err := filesystem.Route("r", []filesystem.Mount{{Prefix: "a", Disk: a}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WriteBytes(ctx, "a/x", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = r.Read(ctx, "z.txt")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = r.Copy(ctx, "z/x", "a/y")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = r.Copy(ctx, "a/x", "z/y")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = r.Move(ctx, "a/x", "z/y")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	wantCode(t, r.SetVisibility(ctx, "z.txt", filesystem.VisibilityPublic), filesystem.CodeNotFound)
	if p, err := r.ListPage(ctx, "z", filesystem.ListOptions{}); err != nil || len(p.Entries) != 0 || p.NextCursor != "" {
		t.Fatalf("listing outside every mount: %v, %v", keysOfPage(p), err)
	}
	if err := r.DeleteKeys(ctx, []string{"z/x", "a/x"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.Exists(ctx, "x"); ok {
		t.Fatal("DeleteKeys skipped the mounted key")
	}
}

// TestRouteListFailsWithAMount proves a listing that must look inside a
// mount to decide whether to show a directory returns that mount's
// listing failure instead of guessing.
func TestRouteListFailsWithAMount(t *testing.T) {
	ctx := t.Context()
	root := mustDisk(t, "root", memory.New())
	deep := mustDisk(t, "deep", memory.New(), filesystem.WithInterceptors(failWhen(func(op *filesystem.Op) bool { return op.Kind == filesystem.OpList })))
	r, err := filesystem.Route("r", []filesystem.Mount{{Disk: root}, {Prefix: "a/b", Disk: deep}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ListPage(ctx, "", filesystem.ListOptions{}); !isBroken(err) {
		t.Fatalf("list = %v, want the mount's failure", err)
	}
}

// TestOverlayRefusesWhiteoutKeys proves reads and deletes of keys in the
// whiteout directory are refused, so a caller can neither read nor remove
// the records that hide lower keys.
func TestOverlayRefusesWhiteoutKeys(t *testing.T) {
	ctx := t.Context()
	o, err := filesystem.Overlay("o", mustDisk(t, "upper", memory.New()), mustDisk(t, "lower", memory.New()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = o.Read(ctx, filesystem.OverlayWhiteouts+"/a")
	wantCode(t, err, filesystem.CodeInvalidKey)
	wantCode(t, o.Delete(ctx, filesystem.OverlayWhiteouts+"/a"), filesystem.CodeInvalidKey)
}

// TestOverlayUpperFailures proves a failing upper disk fails the overlay:
// a whiteout that cannot be checked is not taken as absent (lower's copy
// stays hidden behind the error), and a delete upper refuses writes no
// whiteout.
func TestOverlayUpperFailures(t *testing.T) {
	ctx := t.Context()
	var down atomic.Bool
	upper := mustDisk(t, "upper", memory.New(), filesystem.WithInterceptors(failWhen(func(op *filesystem.Op) bool {
		return down.Load() && (op.Kind == filesystem.OpDelete || op.Kind == filesystem.OpStat && strings.HasPrefix(op.Key, filesystem.OverlayWhiteouts+"/"))
	})))
	lower := mustDisk(t, "lower", memory.New())
	if _, err := lower.WriteBytes(ctx, "k", []byte("lower"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	o, err := filesystem.Overlay("o", upper, lower)
	if err != nil {
		t.Fatal(err)
	}
	down.Store(true)
	if _, err := o.Stat(ctx, "k"); !isBroken(err) {
		t.Errorf("stat with whiteouts unreadable: %v", err)
	}
	if _, err := o.Read(ctx, "k"); !isBroken(err) {
		t.Errorf("read with whiteouts unreadable: %v", err)
	}
	if err := o.Delete(ctx, "k"); !isBroken(err) {
		t.Errorf("delete upper refuses: %v", err)
	}
	down.Store(false)
	if b, err := o.ReadAll(ctx, "k"); err != nil || string(b) != "lower" {
		t.Fatalf("after the failures: %q, %v; want lower's copy still visible", b, err)
	}
}

// TestOverlayListEdges covers the merge's edge cases: a whiteout whose
// lower key is gone shows nothing, an upper page holding only whiteouts
// is skipped, a lower listing failure (at the start, on a later page, or
// inside a whited-out directory) is returned.
func TestOverlayListEdges(t *testing.T) {
	ctx := t.Context()
	var lowerCalls atomic.Int64
	var failFrom atomic.Int64 // fail lower listings from this call on; 0 never
	var failDir atomic.Value
	failDir.Store("\x00")
	lower := mustDisk(t, "lower", memory.New(), filesystem.WithInterceptors(failWhen(func(op *filesystem.Op) bool {
		if op.Kind != filesystem.OpList {
			return false
		}
		n := lowerCalls.Add(1)
		return (failFrom.Load() > 0 && n >= failFrom.Load()) || op.Key == failDir.Load()
	})))
	upper := mustDisk(t, "upper", memory.New())
	for _, k := range []string{"gone", "kept", "d/x", "d/y"} {
		if _, err := lower.WriteBytes(ctx, k, []byte(k), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	o, err := filesystem.Overlay("o", upper, lower)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"gone", "d/x"} {
		if err := o.Delete(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := o.WriteBytes(ctx, "z", []byte("z"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	// The whiteout of "gone" now hides nothing.
	if err := lower.Delete(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	var all []string
	opts := filesystem.ListOptions{Limit: 1}
	for {
		p, err := o.ListPage(ctx, "", opts)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, keysOfPage(p)...)
		if p.NextCursor == "" {
			break
		}
		opts.Cursor = p.NextCursor
	}
	if strings.Join(all, ",") != "d,kept,z" {
		t.Fatalf("listing = %v, want d, kept, z", all)
	}

	failFrom.Store(lowerCalls.Load() + 1)
	if _, err := o.ListPage(ctx, "", filesystem.ListOptions{}); !isBroken(err) {
		t.Errorf("lower listing down at the start: %v", err)
	}
	failFrom.Store(lowerCalls.Load() + 2)
	if _, err := o.ListPage(ctx, "", filesystem.ListOptions{Limit: 1}); !isBroken(err) {
		t.Errorf("lower's second page down: %v", err)
	}
	failFrom.Store(0)
	failDir.Store("d")
	if _, err := o.ListPage(ctx, "", filesystem.ListOptions{}); !isBroken(err) {
		t.Errorf("lower down inside a whited-out directory: %v", err)
	}
}
