package filesystem

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
)

// Mount places a disk at a directory of a routed disk (see Route).
type Mount struct {
	// Prefix is the directory the disk appears under: key "a.png" on Disk
	// is "<Prefix>/a.png" on the routed disk. "" mounts Disk at the root as
	// the catch-all for keys under no other prefix.
	Prefix string
	// Disk serves the keys under Prefix, with its own interceptors,
	// settings and capabilities.
	Disk *Disk
}

// Route returns a Disk named name that dispatches each key to the mount
// with the longest Prefix containing it, so one handle can put "avatars/…"
// on a CDN-backed bucket, "exports/…" on cold storage and everything else
// on a default disk. opts configure the routed disk itself (interceptors
// around every routed call, a URL signer, …); each mounted disk keeps its
// own.
//
// Semantics, which callers must accept:
//   - A key under no prefix, with no catch-all mount, does not exist: Stat
//     and Read report CodeNotFound, Delete succeeds, and Write fails with
//     CodeInvalidArgument.
//   - Keys of the catch-all under a mounted prefix are shadowed: unreachable
//     and not listed, as with a filesystem mount.
//   - Copy and Move inside one mount use that disk (server-side where it
//     can); across mounts they stream (CopyAcross, MoveAcross), and a move
//     across mounts is not atomic.
//   - Listing a directory above mounts (the root, or "a" over a mount at
//     "a/b") merges the catch-all's entries with the mounts, shown as
//     directories, or with their keys when recursive.
//   - The routed disk declares a capability only when every mount has it:
//     Copy, Move, SetVisibility, Presign (the mount can sign both GET and
//     PUT, each through its driver or its URLSigner as WithURLSigner routes
//     that method; signer URLs name the mount), PublicURL (the mount's
//     driver or a WithPublicURL base), PresignHeaders (whichever of the
//     two signs the mount's PUTs can pin headers), PresignIP (whichever
//     signs each method enforces it), PresignVisibility (every mount's
//     PUTs go to a driver that pins it), and the write attributes
//     (Metadata, ContentType, Headers, ConditionalWrite, ChecksumSHA256,
//     CompareAndSwap). DeleteKeys is always declared, each mount deleting
//     its share its own way; CaseInsensitive when any mount folds case.
//     Multipart, form uploads, appends, tags, holds, versions, version
//     reads, storage classes, restores,
//     conditional reads, modification times, bulk DeletePrefix,
//     SweepAbandoned, FindAbandoned and real directories (Dirs) are not
//     routed: use the mounted disk for those (the Disk falls back for
//     DeletePrefix, conditional reads and DirExists, and MakeDir does
//     nothing).
func Route(name string, mounts []Mount, opts ...Option) (*Disk, error) {
	if len(mounts) == 0 {
		return nil, Errorf(CodeInvalidArgument, "route needs at least one mount")
	}
	r := &routeDriver{}
	seen := make(map[string]bool, len(mounts))
	for _, m := range mounts {
		if m.Disk == nil {
			return nil, Errorf(CodeInvalidArgument, "route mount %q has a nil disk", m.Prefix)
		}
		p, err := CleanDir(m.Prefix)
		if err != nil {
			return nil, err
		}
		if seen[p] {
			return nil, Errorf(CodeInvalidArgument, "route mounts prefix %q twice", p)
		}
		seen[p] = true
		if p == "" {
			r.root = m.Disk
			continue
		}
		r.mounts = append(r.mounts, Mount{Prefix: p, Disk: m.Disk})
	}
	// Longest prefix first, so the first match is the deepest mount.
	slices.SortFunc(r.mounts, func(a, b Mount) int { return len(b.Prefix) - len(a.Prefix) })
	return NewDisk(name, r, opts...)
}

// routeDriver dispatches to the mounted disks; see Route.
type routeDriver struct {
	mounts []Mount // longest prefix first
	root   *Disk   // the catch-all, or nil
}

// all returns every mounted disk with the prefix it is mounted at.
func (r *routeDriver) all() []Mount {
	out := slices.Clone(r.mounts)
	if r.root != nil {
		out = append(out, Mount{Disk: r.root})
	}
	return out
}

// resolve returns the mount serving key and key relative to it.
func (r *routeDriver) resolve(key string) (Mount, string, bool) {
	for _, m := range r.mounts {
		if KeyInDir(key, m.Prefix) {
			return m, RelativeKey(key, m.Prefix), true
		}
	}
	if r.root != nil {
		return Mount{Disk: r.root}, key, true
	}
	return Mount{}, "", false
}

// shadowed reports whether an entry of base lies under a deeper mount (or
// is the directory such a mount replaces), so the routed disk cannot show
// it.
func (r *routeDriver) shadowed(e Entry, base Mount) bool {
	for _, m := range r.mounts {
		if len(m.Prefix) <= len(base.Prefix) {
			continue
		}
		if KeyInDir(e.Key, m.Prefix) || (e.IsDir && e.Key == m.Prefix) {
			return true
		}
	}
	return false
}

func (r *routeDriver) Capabilities() Capabilities {
	c := Capabilities{
		Copy: true, Move: true, SetVisibility: true, DeleteKeys: true, Presign: true, PublicURL: true,
		Metadata: true, ContentType: true, Headers: true, PresignHeaders: true, PresignIP: true, PresignVisibility: true,
		ConditionalWrite: true, ChecksumSHA256: true, CompareAndSwap: true,
	}
	for _, m := range r.all() {
		mc := m.Disk.caps
		presign, presignIP, presignHeaders := m.Disk.signing()
		c.Copy = c.Copy && mc.Copy
		c.Move = c.Move && mc.Move
		c.SetVisibility = c.SetVisibility && mc.SetVisibility
		c.Presign = c.Presign && presign
		c.PublicURL = c.PublicURL && (mc.PublicURL || m.Disk.publicBase != "")
		c.Metadata = c.Metadata && mc.Metadata
		c.ContentType = c.ContentType && mc.ContentType
		c.Headers = c.Headers && mc.Headers
		c.PresignHeaders = c.PresignHeaders && presignHeaders
		c.PresignIP = c.PresignIP && presignIP
		c.PresignVisibility = c.PresignVisibility && m.Disk.presignsVisibility()
		c.ConditionalWrite = c.ConditionalWrite && mc.ConditionalWrite
		c.ChecksumSHA256 = c.ChecksumSHA256 && mc.ChecksumSHA256
		c.CompareAndSwap = c.CompareAndSwap && mc.CompareAndSwap
		c.CaseInsensitive = c.CaseInsensitive || mc.CaseInsensitive
	}
	c.PresignHeaders = c.PresignHeaders && c.Presign && c.Headers
	c.PresignIP = c.PresignIP && c.Presign
	c.PresignVisibility = c.PresignVisibility && c.Presign && c.SetVisibility
	return c
}

// errNoMount is what a write to a key outside every mount fails with.
func errNoMount(key string) error {
	return Errorf(CodeInvalidArgument, "no mount serves key %q", key)
}

// rebase gives an entry from a mount its key on the routed disk.
func rebase(e Entry, prefix string) Entry {
	e.Key = JoinKey(prefix, e.Key)
	return e
}

func (r *routeDriver) Stat(ctx context.Context, key string) (Entry, error) {
	m, rel, ok := r.resolve(key)
	if !ok {
		return Entry{}, Errorf(CodeNotFound, "no mount serves key %q", key)
	}
	e, err := m.Disk.Stat(ctx, rel)
	return rebase(e, m.Prefix), err
}

func (r *routeDriver) Read(ctx context.Context, key string, rng ByteRange) (io.ReadCloser, error) {
	m, rel, ok := r.resolve(key)
	if !ok {
		return nil, Errorf(CodeNotFound, "no mount serves key %q", key)
	}
	return m.Disk.ReadRange(ctx, rel, rng)
}

func (r *routeDriver) Write(ctx context.Context, key string, body io.Reader, opts WriteOptions) (Entry, error) {
	m, rel, ok := r.resolve(key)
	if !ok {
		return Entry{}, errNoMount(key)
	}
	e, err := m.Disk.Write(ctx, rel, body, opts)
	return rebase(e, m.Prefix), err
}

func (r *routeDriver) Delete(ctx context.Context, key string) error {
	m, rel, ok := r.resolve(key)
	if !ok {
		return nil
	}
	return m.Disk.Delete(ctx, rel)
}

// List lists dir on the deepest mount containing it, merged with the
// mounts below dir (shown as directories, or with their keys when
// recursive) while hiding that mount's keys they shadow.
func (r *routeDriver) List(ctx context.Context, dir string, opts ListOptions) (Page, error) {
	base, baseDir, ok := r.containing(dir)
	var below []Mount
	for _, m := range r.mounts {
		if KeyInDir(m.Prefix, dir) {
			below = append(below, m)
		}
	}
	var sources []listSource
	if ok {
		sources = append(sources, listSource{
			page: func(ctx context.Context, cursor string) (Page, error) {
				o := opts
				o.Cursor = cursor
				page, err := base.Disk.ListPage(ctx, baseDir, o)
				for i := range page.Entries {
					page.Entries[i] = rebase(page.Entries[i], base.Prefix)
				}
				return page, err
			},
			keep: func(e Entry) bool { return !r.shadowed(e, base) },
		})
	}
	if len(below) == 0 {
		if !ok {
			return Page{}, nil
		}
		// Nothing below dir is mounted: the base's own page and cursor.
		return sources[0].page(ctx, opts.Cursor)
	}
	if opts.Recursive {
		slices.SortFunc(below, func(a, b Mount) int { return strings.Compare(a.Prefix, b.Prefix) })
		for _, m := range below {
			sources = append(sources, listSource{
				page: func(ctx context.Context, cursor string) (Page, error) {
					page, err := m.Disk.ListPage(ctx, "", ListOptions{Recursive: true, Cursor: cursor, Limit: opts.Limit})
					for i := range page.Entries {
						page.Entries[i] = rebase(page.Entries[i], m.Prefix)
					}
					return page, err
				},
				// A deeper mount inside m shadows m's keys there.
				keep: func(e Entry) bool { return !r.shadowed(e, m) },
			})
		}
	} else {
		var dirs []Entry
		for _, m := range below {
			first, _, _ := strings.Cut(RelativeKey(m.Prefix, dir), separator)
			sub := JoinKey(dir, first)
			if !slices.ContainsFunc(dirs, func(e Entry) bool { return e.Key == sub }) {
				dirs = append(dirs, Entry{Key: sub, IsDir: true})
			}
		}
		slices.SortFunc(dirs, func(a, b Entry) int { return strings.Compare(a.Key, b.Key) })
		sources = append(sources, listSource{page: func(context.Context, string) (Page, error) {
			return Page{Entries: dirs}, nil
		}})
	}
	return mergeLists(ctx, sources, opts, r.show)
}

// show shows a key's entry from the first source that has it, except a
// directory that is a mount or holds one: directories exist only while
// they hold a visible key, and its keys may all be shadowed by an empty
// mount, so it is shown only when a recursive listing of it finds one.
func (r *routeDriver) show(ctx context.Context, heads []*Entry) (Entry, bool, error) {
	e, ok, _ := firstHead(ctx, heads)
	if !ok || !e.IsDir || !slices.ContainsFunc(r.mounts, func(m Mount) bool {
		return m.Prefix == e.Key || KeyInDir(m.Prefix, e.Key)
	}) {
		return e, ok, nil
	}
	page, err := r.List(ctx, e.Key, ListOptions{Recursive: true, Limit: 1})
	if err != nil {
		return Entry{}, false, err
	}
	return e, len(page.Entries) > 0, nil
}

// containing returns the deepest mount whose prefix is dir or contains it
// (else the catch-all) and dir relative to it.
func (r *routeDriver) containing(dir string) (Mount, string, bool) {
	for _, m := range r.mounts {
		if dir == m.Prefix {
			return m, "", true
		}
		if KeyInDir(dir, m.Prefix) {
			return m, RelativeKey(dir, m.Prefix), true
		}
	}
	if r.root != nil {
		return Mount{Disk: r.root}, dir, true
	}
	return Mount{}, "", false
}

// firstHead shows a key's entry from the first source that has it.
func firstHead(_ context.Context, heads []*Entry) (Entry, bool, error) {
	for _, h := range heads {
		if h != nil {
			return *h, true, nil
		}
	}
	return Entry{}, false, nil
}

// pair resolves the two keys of a copy or move.
func (r *routeDriver) pair(src, dst string) (Mount, string, Mount, string, error) {
	sm, srel, ok := r.resolve(src)
	if !ok {
		return Mount{}, "", Mount{}, "", Errorf(CodeNotFound, "no mount serves key %q", src)
	}
	dm, drel, ok := r.resolve(dst)
	if !ok {
		return Mount{}, "", Mount{}, "", errNoMount(dst)
	}
	return sm, srel, dm, drel, nil
}

// Copy copies inside a mount with its disk, across mounts with CopyAcross.
func (r *routeDriver) Copy(ctx context.Context, src, dst string) (Entry, error) {
	sm, srel, dm, drel, err := r.pair(src, dst)
	if err != nil {
		return Entry{}, err
	}
	e, err := CopyAcross(ctx, sm.Disk, srel, dm.Disk, drel)
	return rebase(e, dm.Prefix), err
}

// Move moves inside a mount with its disk, across mounts with MoveAcross.
func (r *routeDriver) Move(ctx context.Context, src, dst string) (Entry, error) {
	sm, srel, dm, drel, err := r.pair(src, dst)
	if err != nil {
		return Entry{}, err
	}
	e, err := MoveAcross(ctx, sm.Disk, srel, dm.Disk, drel)
	return rebase(e, dm.Prefix), err
}

func (r *routeDriver) SetVisibility(ctx context.Context, key string, v Visibility) error {
	m, rel, ok := r.resolve(key)
	if !ok {
		return Errorf(CodeNotFound, "no mount serves key %q", key)
	}
	return m.Disk.SetVisibility(ctx, rel, v)
}

// DeleteKeys hands each mount its share of keys; keys under no mount are
// missing and so deleted. Failed keys come back as routed keys.
func (r *routeDriver) DeleteKeys(ctx context.Context, keys []string) error {
	type share struct {
		mount Mount
		keys  []string
	}
	var shares []*share
	for _, k := range keys {
		m, rel, ok := r.resolve(k)
		if !ok {
			continue
		}
		i := slices.IndexFunc(shares, func(s *share) bool { return s.mount == m })
		if i < 0 {
			shares = append(shares, &share{mount: m})
			i = len(shares) - 1
		}
		shares[i].keys = append(shares[i].keys, rel)
	}
	var failed []KeyError
	for _, s := range shares {
		// Disk.DeleteKeys reports every failure, a whole-call one
		// included, as a *BatchError, so nil or a batch is all it returns.
		var batch *BatchError
		if errors.As(s.mount.Disk.DeleteKeys(ctx, s.keys), &batch) {
			for _, f := range batch.Failures {
				failed = append(failed, KeyError{Key: JoinKey(s.mount.Prefix, f.Key), Err: f.Err})
			}
		}
	}
	if len(failed) > 0 {
		return &BatchError{Failures: failed}
	}
	return nil
}

// Presign signs with the mount: its driver, or its URLSigner, whose URL
// names the mounted disk and the key on it.
func (r *routeDriver) Presign(ctx context.Context, key string, opts PresignOptions) (SignedRequest, error) {
	m, rel, ok := r.resolve(key)
	if !ok {
		return SignedRequest{}, errNoMount(key)
	}
	return m.Disk.SignedURL(ctx, rel, opts)
}

func (r *routeDriver) PublicURL(ctx context.Context, key string) (string, error) {
	m, rel, ok := r.resolve(key)
	if !ok {
		return "", errNoMount(key)
	}
	return m.Disk.PublicURL(ctx, rel)
}
