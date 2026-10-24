package filesystemtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
)

// dirCases are the cases for directories (MakeDir, DirExists) and for the
// dry run of the abandoned-work sweep (FindAbandoned).
func dirCases() []Case {
	return []Case{
		{Name: "DirExists", Check: dirExists},
		{Name: "MakeDir", Needs: func(c filesystem.Capabilities) bool { return c.Dirs }, Check: makeDir},
		{Name: "MakeDirWithoutDirsWritesNothing", Needs: func(c filesystem.Capabilities) bool { return !c.Dirs }, Check: makeDirWritesNothing},
		{Name: "FindAbandoned", Needs: func(c filesystem.Capabilities) bool { return c.FindAbandoned && c.Multipart }, Check: findAbandoned},
		{Name: "FindAbandonedKeepsObjects", Needs: func(c filesystem.Capabilities) bool { return c.FindAbandoned && !c.Multipart }, Check: findAbandonedKeepsObjects},
		{Name: "FindAbandonedUnsupportedIsRefused", Needs: func(c filesystem.Capabilities) bool { return !c.FindAbandoned }, Check: findAbandonedRefused},
	}
}

// expectDir checks DirExists(dir) against want.
func expectDir(ctx context.Context, d *filesystem.Disk, dir string, want bool) error {
	got, err := d.DirExists(ctx, dir)
	if err != nil {
		return fmt.Errorf("DirExists(%q): %w", dir, err)
	}
	if got != want {
		return fmt.Errorf("DirExists(%q) = %v, want %v", dir, got, want)
	}
	return nil
}

// dirExists holds on every driver: a directory exists while a key is under
// it, an object's key is not a directory, and the root always exists.
func dirExists(ctx context.Context, d *filesystem.Disk) error {
	const key = "dirs/a/x.txt"
	if err := put(ctx, d, key, "x"); err != nil {
		return err
	}
	for dir, want := range map[string]bool{"": true, "dirs": true, "dirs/a": true, "dirs/a/x.txt": false, "dirs/none": false, "dir": false} {
		if err := expectDir(ctx, d, dir, want); err != nil {
			return err
		}
	}
	if err := d.DeletePrefix(ctx, "dirs"); err != nil {
		return fmt.Errorf("delete prefix: %w", err)
	}
	if err := expectDir(ctx, d, "dirs/a", false); err != nil {
		return fmt.Errorf("after DeletePrefix: %w", err)
	}
	return expectDir(ctx, d, "dirs", false)
}

// makeDir is the case for drivers with real directories: an empty
// directory exists, lists as empty, takes keys, cannot replace an object,
// and goes with DeletePrefix.
func makeDir(ctx context.Context, d *filesystem.Disk) error {
	const dir, object = "made/a/b", "made/file.txt"
	for range 2 { // idempotent
		if err := d.MakeDir(ctx, dir); err != nil {
			return fmt.Errorf("MakeDir(%q): %w", dir, err)
		}
	}
	for _, p := range []string{dir, "made/a", "made"} {
		if err := expectDir(ctx, d, p, true); err != nil {
			return err
		}
	}
	if err := expectKeys(ctx, d, dir, filesystem.ListOptions{Recursive: true}); err != nil {
		return fmt.Errorf("an empty directory must list no keys: %w", err)
	}
	if err := put(ctx, d, object, "x"); err != nil {
		return err
	}
	if err := expectCode("MakeDir over an object", d.MakeDir(ctx, object), filesystem.CodeExists); err != nil {
		return err
	}
	if err := put(ctx, d, dir+"/f.txt", "inside"); err != nil {
		return err
	}
	if err := expect(ctx, d, dir+"/f.txt", "inside"); err != nil {
		return err
	}
	if err := d.DeletePrefix(ctx, "made"); err != nil {
		return fmt.Errorf("delete prefix: %w", err)
	}
	for _, p := range []string{dir, "made"} {
		if err := expectDir(ctx, d, p, false); err != nil {
			return fmt.Errorf("after DeletePrefix: %w", err)
		}
	}
	return nil
}

// makeDirWritesNothing is the case for object stores: MakeDir succeeds and
// writes no marker object, so the directory appears only with a key.
func makeDirWritesNothing(ctx context.Context, d *filesystem.Disk) error {
	const dir = "nodirs/a"
	// The Disk calls no driver method here, so only an interceptor could
	// fail MakeDir; its error and the listing are checked together.
	if err := errors.Join(d.MakeDir(ctx, dir), expectKeys(ctx, d, "nodirs", filesystem.ListOptions{Recursive: true})); err != nil {
		return fmt.Errorf("MakeDir without the Dirs capability: %w", err)
	}
	if err := expectDir(ctx, d, dir, false); err != nil {
		return err
	}
	if err := put(ctx, d, dir+"/f.txt", "x"); err != nil {
		return err
	}
	return expectDir(ctx, d, dir, true)
}

// findAbandoned checks the dry run against the sweep: it reports the
// abandoned upload under the same cutoff rules, removes nothing, and no
// longer reports it once swept.
func findAbandoned(ctx context.Context, d *filesystem.Disk) error {
	const key = "mp/preview.bin"
	// The key the driver receives, which the preview reports.
	want, kerr := d.DriverKey(key)
	id, err := d.CreateMultipart(ctx, key, filesystem.WriteOptions{})
	if err = errors.Join(kerr, err); err != nil {
		return err
	}
	if _, err := d.UploadPart(ctx, key, id, 1, strings.NewReader("data"), 4); err != nil {
		return err
	}
	if found, err := d.FindAbandoned(ctx, time.Now().Add(-sweepMargin)); err != nil || hasUpload(found, id) {
		return fmt.Errorf("a preview with an older cutoff reported an upload in progress (found %v, err %v)", found, err)
	}
	cutoff := time.Now().Add(sweepMargin)
	for range 2 { // a preview changes nothing, so it repeats
		found, err := d.FindAbandoned(ctx, cutoff)
		if err != nil {
			return fmt.Errorf("preview: %w", err)
		}
		i := slices.IndexFunc(found, func(a filesystem.Abandoned) bool { return a.ID == id })
		if i < 0 {
			return fmt.Errorf("preview %v does not report abandoned upload %q", found, id)
		}
		if a := found[i]; a.Kind != filesystem.AbandonedMultipart || a.Key != want || !a.LastActivity.Before(cutoff) || a.LastActivity.IsZero() {
			return fmt.Errorf("preview reported %+v, want a multipart upload of %q last active before %v", a, want, cutoff)
		}
	}
	if parts, err := d.ListParts(ctx, key, id); err != nil || len(parts) != 1 {
		return fmt.Errorf("a preview removed the upload it reported (parts %v, err %v)", parts, err)
	}
	if n, err := d.SweepAbandoned(ctx, cutoff); err != nil || n < 1 {
		return fmt.Errorf("sweep after the preview: removed %d, err %v", n, err)
	}
	found, err := d.FindAbandoned(ctx, cutoff)
	if err != nil || hasUpload(found, id) {
		return fmt.Errorf("a preview after the sweep still reports the upload (found %v, err %v)", found, err)
	}
	return nil
}

// hasUpload reports whether found holds the upload id.
func hasUpload(found []filesystem.Abandoned, id string) bool {
	return slices.ContainsFunc(found, func(a filesystem.Abandoned) bool { return a.ID == id })
}

// findAbandonedKeepsObjects is the preview case for drivers whose only
// leftovers are staging files: a preview that counts everything as
// abandoned succeeds, reports no stored object, and removes nothing.
func findAbandonedKeepsObjects(ctx context.Context, d *filesystem.Disk) error {
	const kept = "preview/kept.txt"
	if err := put(ctx, d, kept, "stored"); err != nil {
		return err
	}
	found, err := d.FindAbandoned(ctx, time.Now().Add(sweepMargin))
	if err != nil {
		return fmt.Errorf("preview: %w", err)
	}
	for _, a := range found {
		if a.Kind != filesystem.AbandonedStaging || a.Key != "" {
			return fmt.Errorf("a driver without multipart reported %+v, want only staging files", a)
		}
	}
	return expect(ctx, d, kept, "stored")
}

func findAbandonedRefused(ctx context.Context, d *filesystem.Disk) error {
	if _, err := d.FindAbandoned(ctx, time.Now()); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		return fmt.Errorf("FindAbandoned without the capability: err %v, want unsupported", err)
	}
	return nil
}
