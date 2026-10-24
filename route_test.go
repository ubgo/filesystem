package filesystem_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/filesystemtest"
	"github.com/ubgo/filesystem/memory"
)

// newRoute mounts memory disks at "a" and "batch" (prefixes the
// conformance suite writes under) over a memory catch-all.
func newRoute(t *testing.T) (r, root, a, batch *filesystem.Disk) {
	t.Helper()
	root = mustDisk(t, "root", memory.New())
	a = mustDisk(t, "a", memory.New())
	batch = mustDisk(t, "batch", memory.New())
	r, err := filesystem.Route("routed", []filesystem.Mount{{Disk: root}, {Prefix: "a", Disk: a}, {Prefix: "batch", Disk: batch}})
	if err != nil {
		t.Fatal(err)
	}
	return r, root, a, batch
}

func TestRouteConformance(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		r, _, _, _ := newRoute(t)
		return r
	})
}

func TestRouteDispatchesByLongestPrefix(t *testing.T) {
	ctx := t.Context()
	root := mustDisk(t, "root", memory.New())
	a := mustDisk(t, "a", memory.New())
	ab := mustDisk(t, "ab", memory.New())
	r, err := filesystem.Route("r", []filesystem.Mount{{Disk: root}, {Prefix: "a", Disk: a}, {Prefix: "a/b", Disk: ab}})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"x.txt", "a/y.txt", "a/b/z.txt", "a/bb/w.txt"} {
		if _, err := r.WriteBytes(ctx, k, []byte(k), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for d, want := range map[*filesystem.Disk][]string{root: {"x.txt"}, a: {"bb/w.txt", "y.txt"}, ab: {"z.txt"}} {
		if got := allKeys(t, d, ""); !slices.Equal(got, want) {
			t.Errorf("%s holds %v, want %v", d.Name(), got, want)
		}
	}
	e, err := r.Stat(ctx, "a/b/z.txt")
	if err != nil || e.Key != "a/b/z.txt" {
		t.Errorf("stat through the route: %+v, %v", e, err)
	}

	// The root merges the catch-all with the mounts as directories, and a
	// directory above a nested mount shows it too.
	if got := keysOf(t, r, "", false); !slices.Equal(got, []string{"a/", "x.txt"}) {
		t.Errorf("root listing %v", got)
	}
	if got := keysOf(t, r, "", true); !slices.Equal(got, []string{"a/b/z.txt", "a/bb/w.txt", "a/y.txt", "x.txt"}) {
		t.Errorf("recursive root listing %v", got)
	}
	if got := keysOf(t, r, "a", false); !slices.Equal(got, []string{"a/b/", "a/bb/", "a/y.txt"}) {
		t.Errorf("listing above a nested mount %v", got)
	}

	// Paging through a merged listing, whatever the page size, neither
	// loses nor repeats a key, including when a page ends inside a
	// mount's own page.
	for limit := 1; limit <= 4; limit++ {
		var paged []string
		opts := filesystem.ListOptions{Recursive: true, Limit: limit}
		for {
			page, err := r.ListPage(ctx, "", opts)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range page.Entries {
				paged = append(paged, e.Key)
			}
			if page.NextCursor == "" {
				break
			}
			opts.Cursor = page.NextCursor
		}
		if !slices.Equal(paged, []string{"a/b/z.txt", "a/bb/w.txt", "a/y.txt", "x.txt"}) {
			t.Errorf("paged by %d: %v", limit, paged)
		}
	}
	if _, err := r.ListPage(ctx, "", filesystem.ListOptions{Cursor: "forged"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("forged cursor: %v", err)
	}
}

func TestRouteShadowsAndRefusesUnmounted(t *testing.T) {
	ctx := t.Context()
	root := mustDisk(t, "root", memory.New())
	a := mustDisk(t, "a", memory.New())
	// A catch-all key under a mounted prefix is hidden by the mount.
	if _, err := root.WriteBytes(ctx, "a/hidden.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	r, err := filesystem.Route("r", []filesystem.Mount{{Disk: root}, {Prefix: "a", Disk: a}})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.Exists(ctx, "a/hidden.txt"); ok {
		t.Error("shadowed catch-all key is reachable")
	}
	if got := keysOf(t, r, "", true); len(got) != 0 {
		t.Errorf("shadowed key listed: %v", got)
	}

	// A directory whose only keys are shadowed by an empty mount is not
	// listed.
	if _, err := root.WriteBytes(ctx, "x/y/hidden.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	xy, err := filesystem.Route("xy", []filesystem.Mount{{Disk: root}, {Prefix: "x/y", Disk: mustDisk(t, "empty", memory.New())}})
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(t, xy, "", false); !slices.Equal(got, []string{"a/"}) {
		t.Errorf("root over an empty mount lists %v", got)
	}

	// Without a catch-all, keys outside every mount do not exist.
	only, err := filesystem.Route("only", []filesystem.Mount{{Prefix: "a", Disk: a}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := only.WriteBytes(ctx, "elsewhere.txt", []byte("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("write outside every mount: %v", err)
	}
	if _, err := only.Stat(ctx, "elsewhere.txt"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("stat outside every mount: %v", err)
	}
	if err := only.Delete(ctx, "elsewhere.txt"); err != nil {
		t.Errorf("delete outside every mount: %v", err)
	}

	for _, bad := range [][]filesystem.Mount{nil, {{Prefix: "a"}}, {{Prefix: "a", Disk: a}, {Prefix: "a/", Disk: root}}, {{Prefix: "../x", Disk: a}}} {
		if _, err := filesystem.Route("bad", bad); err == nil {
			t.Errorf("route %v accepted", bad)
		}
	}
}

func TestRouteCopiesAcrossMounts(t *testing.T) {
	ctx := t.Context()
	r, root, a, _ := newRoute(t)
	if _, err := r.WriteBytes(ctx, "x.txt", []byte("hello"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	e, err := r.Copy(ctx, "x.txt", "a/copy.txt")
	if err != nil || e.Key != "a/copy.txt" {
		t.Fatalf("copy across mounts: %+v, %v", e, err)
	}
	if b, _ := a.ReadAll(ctx, "copy.txt"); string(b) != "hello" {
		t.Errorf("copy landed as %q", b)
	}
	if _, err := r.Move(ctx, "a/copy.txt", "moved.txt"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.Exists(ctx, "copy.txt"); ok {
		t.Error("move across mounts left the source")
	}
	if b, _ := root.ReadAll(ctx, "moved.txt"); string(b) != "hello" {
		t.Errorf("move landed as %q", b)
	}
}

func TestRouteDeleteKeysReportsRoutedKeys(t *testing.T) {
	ctx := t.Context()
	failing := mustDisk(t, "failing", &failingDeletes{Driver: memory.New()})
	root := mustDisk(t, "root", memory.New())
	r, err := filesystem.Route("r", []filesystem.Mount{{Disk: root}, {Prefix: "f", Disk: failing}})
	if err != nil {
		t.Fatal(err)
	}
	err = r.DeleteKeys(ctx, []string{"ok.txt", "f/x.txt", "f/y.txt"})
	var batch *filesystem.BatchError
	if !errors.As(err, &batch) {
		t.Fatalf("want a BatchError, got %v", err)
	}
	var keys []string
	for _, f := range batch.Failures {
		keys = append(keys, f.Key)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"f/x.txt", "f/y.txt"}) {
		t.Errorf("failed keys %v", keys)
	}
}

func TestRouteCapabilitiesAreTheIntersection(t *testing.T) {
	signer := mustDisk(t, "signing", fullDriver{Driver: memory.New()})
	plain := mustDisk(t, "plain", newBasic())
	r, err := filesystem.Route("r", []filesystem.Mount{{Disk: signer}, {Prefix: "p", Disk: plain}})
	if err != nil {
		t.Fatal(err)
	}
	c := r.Capabilities()
	if c.Presign || c.PublicURL || c.Copy || c.Metadata || !c.DeleteKeys {
		t.Errorf("route declares %v", c)
	}
	alone, err := filesystem.Route("r", []filesystem.Mount{{Disk: signer}})
	if err != nil {
		t.Fatal(err)
	}
	if c := alone.Capabilities(); !c.Presign || !c.PublicURL {
		t.Errorf("route over a signing disk declares %v", c)
	}
}

// keysOf lists dir through ListPage, marking directories with a trailing
// slash.
func keysOf(t *testing.T, d *filesystem.Disk, dir string, recursive bool) []string {
	t.Helper()
	var out []string
	for e, err := range d.List(t.Context(), dir, filesystem.ListOptions{Recursive: recursive}) {
		if err != nil {
			t.Fatal(err)
		}
		if e.IsDir {
			out = append(out, e.Key+"/")
			continue
		}
		out = append(out, e.Key)
	}
	return out
}

// allKeys lists every key under dir.
func allKeys(t *testing.T, d *filesystem.Disk, dir string) []string {
	t.Helper()
	return keysOf(t, d, dir, true)
}

// failingDeletes is a memory driver whose deletes fail transiently, with
// no bulk delete, so the Disk deletes key by key.
type failingDeletes struct{ *memory.Driver }

func (f *failingDeletes) Capabilities() filesystem.Capabilities {
	c := f.Driver.Capabilities()
	c.DeleteKeys, c.DeletePrefix = false, false
	return c
}

func (f *failingDeletes) Delete(context.Context, string) error { return filesystem.ErrUnavailable }

// TestRouteURLsUseTheMount pins that a routed disk signs and builds public
// URLs with the disk mounted at the key's prefix, passing it the key
// relative to the mount, and refuses a key no mount serves.
func TestRouteURLsUseTheMount(t *testing.T) {
	ctx := t.Context()
	root := mustDisk(t, "root", memory.New(), filesystem.WithURLSigner(stubSigner{}), filesystem.WithPublicURL("https://cdn.test"))
	a := mustDisk(t, "a", fullDriver{memory.New()})
	r, err := filesystem.Route("routed", []filesystem.Mount{{Disk: root}, {Prefix: "a", Disk: a}})
	if err != nil {
		t.Fatal(err)
	}
	get := filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute}
	cases := []struct{ key, signed, public string }{
		{"a/x", "https://backend.test/x?sig=1", "https://bucket.test/x"},
		{"b/x", "https://app.test/files/root/b/x", "https://cdn.test/b/x"},
	}
	for _, c := range cases {
		if s, err := r.SignedURL(ctx, c.key, get); err != nil || s.URL != c.signed {
			t.Errorf("SignedURL(%s) = %q, %v; want %q", c.key, s.URL, err, c.signed)
		}
		if u, err := r.PublicURL(ctx, c.key); err != nil || u != c.public {
			t.Errorf("PublicURL(%s) = %q, %v; want %q", c.key, u, err, c.public)
		}
	}

	only, err := filesystem.Route("only", []filesystem.Mount{{Prefix: "a", Disk: a}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := only.SignedURL(ctx, "b/x", get); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("signed URL for an unmounted key: %v", err)
	}
	if _, err := only.PublicURL(ctx, "b/x"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("public URL for an unmounted key: %v", err)
	}
}
