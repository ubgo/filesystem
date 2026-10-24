package filesystem

import (
	"context"
	"io"
	"strings"
)

// OverlayWhiteouts is the directory of the upper disk where an overlay
// records deleted lower keys: deleting "a/b.txt" while the lower disk holds
// it writes an empty "<OverlayWhiteouts>/a/b.txt" on the upper disk. Keys
// under it, in any letter case, are reserved on an overlay disk and
// refused with CodeInvalidKey: on an upper disk that folds case, a key
// such as ".OVERLAY-WHITEOUTS/a.txt" would otherwise forge a whiteout.
const OverlayWhiteouts = ".overlay-whiteouts"

// Overlay returns a Disk named name that layers upper over lower, copy on
// write: reads find a key on upper first and fall through to lower, writes
// go to upper only, and deleting a key lower holds leaves a whiteout on
// upper that hides it. lower is never changed through the overlay. It is
// how a preview environment, a test or a tenant sandbox starts from shared
// read-only data and keeps its changes to itself. opts configure the
// overlay disk itself; upper and lower keep their own interceptors.
//
// Semantics, which callers must accept:
//   - Listings merge both disks in key order, upper winning a key both
//     hold, and hide whited-out keys and directories whose lower keys are
//     all whited out (checking such a directory costs one more listing).
//   - Not atomic across the two disks: a delete removes upper's copy, then
//     writes the whiteout; if that fails, lower's copy shows again and the
//     error is returned, and retrying converges.
//   - The overlay declares only what it honours on both layers: the write
//     attributes upper stores (Metadata, ContentType, Headers,
//     ChecksumSHA256) and CaseInsensitive when either folds case. Copy and
//     Move stream through the Disk (a move of a lower key copies it up and
//     whites it out), DeletePrefix and DeleteKeys go key by key, and the
//     rest (URLs, visibility, conditional and in-place writes, multipart)
//     is not offered: an object can sit on either layer.
func Overlay(name string, upper, lower *Disk, opts ...Option) (*Disk, error) {
	if upper == nil || lower == nil || upper == lower {
		return nil, Errorf(CodeInvalidArgument, "overlay needs two distinct, non-nil disks")
	}
	return NewDisk(name, &overlayDriver{upper: upper, lower: lower}, opts...)
}

// overlayDriver implements Overlay.
type overlayDriver struct {
	upper, lower *Disk
}

// Overlay merge sources, in priority order.
const (
	overlayUpper = iota
	overlayLower
	overlayWhiteout
)

func (o *overlayDriver) Capabilities() Capabilities {
	u := o.upper.caps
	return Capabilities{
		Metadata: u.Metadata, ContentType: u.ContentType, Headers: u.Headers, ChecksumSHA256: u.ChecksumSHA256,
		CaseInsensitive: u.CaseInsensitive || o.lower.caps.CaseInsensitive,
	}
}

// reserved refuses keys in the whiteout directory, comparing its name
// without regard to case so a case-folding upper disk cannot be tricked.
func reserved(key string) error {
	first, _, _ := strings.Cut(key, separator)
	if strings.EqualFold(first, OverlayWhiteouts) {
		return Errorf(CodeInvalidKey, "key %q is reserved for overlay whiteouts", key)
	}
	return nil
}

// whiteout is the upper key that records key's deletion.
func whiteout(key string) string { return JoinKey(OverlayWhiteouts, key) }

// whitedOut reports whether key's lower copy is hidden.
func (o *overlayDriver) whitedOut(ctx context.Context, key string) (bool, error) {
	return o.upper.Exists(ctx, whiteout(key))
}

func (o *overlayDriver) Stat(ctx context.Context, key string) (Entry, error) {
	if err := reserved(key); err != nil {
		return Entry{}, err
	}
	e, err := o.upper.Stat(ctx, key)
	if CodeOf(err) != CodeNotFound {
		return e, err
	}
	if hidden, werr := o.whitedOut(ctx, key); werr != nil || hidden {
		return Entry{}, firstErr(werr, err)
	}
	return o.lower.Stat(ctx, key)
}

func (o *overlayDriver) Read(ctx context.Context, key string, rng ByteRange) (io.ReadCloser, error) {
	if err := reserved(key); err != nil {
		return nil, err
	}
	rc, err := o.upper.ReadRange(ctx, key, rng)
	if CodeOf(err) != CodeNotFound {
		return rc, err
	}
	if hidden, werr := o.whitedOut(ctx, key); werr != nil || hidden {
		return nil, firstErr(werr, err)
	}
	return o.lower.ReadRange(ctx, key, rng)
}

// firstErr returns a when it is set, else b.
func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// Write stores on upper, then removes any whiteout, so the key shows again.
func (o *overlayDriver) Write(ctx context.Context, key string, body io.Reader, opts WriteOptions) (Entry, error) {
	if err := reserved(key); err != nil {
		return Entry{}, err
	}
	e, err := o.upper.Write(ctx, key, body, opts)
	if err != nil {
		return Entry{}, err
	}
	return e, o.upper.Delete(ctx, whiteout(key))
}

// Delete removes upper's copy and whites out lower's, if lower has one.
func (o *overlayDriver) Delete(ctx context.Context, key string) error {
	if err := reserved(key); err != nil {
		return err
	}
	if err := o.upper.Delete(ctx, key); err != nil {
		return err
	}
	inLower, err := o.lower.Exists(ctx, key)
	if err != nil || !inLower {
		return err
	}
	_, err = o.upper.WriteBytes(ctx, whiteout(key), nil, WriteOptions{})
	return err
}

// List merges upper, lower and upper's whiteouts for dir.
func (o *overlayDriver) List(ctx context.Context, dir string, opts ListOptions) (Page, error) {
	if err := reserved(dir); err != nil {
		return Page{}, err
	}
	at := func(d *Disk, dir string, strip string) listSource {
		return listSource{page: func(ctx context.Context, cursor string) (Page, error) {
			lo := opts
			lo.Cursor = cursor
			page, err := d.ListPage(ctx, dir, lo)
			for i := range page.Entries {
				page.Entries[i].Key = RelativeKey(page.Entries[i].Key, strip)
			}
			return page, err
		}}
	}
	upper := at(o.upper, dir, "")
	upper.keep = func(e Entry) bool { return reserved(e.Key) == nil }
	sources := []listSource{
		overlayUpper:    upper,
		overlayLower:    at(o.lower, dir, ""),
		overlayWhiteout: at(o.upper, JoinKey(OverlayWhiteouts, dir), OverlayWhiteouts),
	}
	return mergeLists(ctx, sources, opts, o.resolve)
}

// resolve shows upper's entry, else lower's unless a whiteout hides it.
func (o *overlayDriver) resolve(ctx context.Context, heads []*Entry) (Entry, bool, error) {
	u, l, w := heads[overlayUpper], heads[overlayLower], heads[overlayWhiteout]
	switch {
	case u != nil:
		return *u, true, nil
	case l == nil:
		// A whiteout with nothing left to hide.
		return Entry{}, false, nil
	case w == nil || l.IsDir != w.IsDir:
		return *l, true, nil
	case !l.IsDir:
		return Entry{}, false, nil
	}
	// A lower directory with whiteouts inside shows while any key in it
	// is still visible.
	page, err := o.List(ctx, l.Key, ListOptions{Recursive: true, Limit: 1})
	if err != nil {
		return Entry{}, false, err
	}
	return *l, len(page.Entries) > 0, nil
}
