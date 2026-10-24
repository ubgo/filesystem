package filesystem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"reflect"
	"strings"
)

// SyncAction is what Sync does with one object. It is a closed set
// (SyncActions lists it), so a report consumer can switch on it
// exhaustively. There is no "skip" action: an unchanged object has no
// SyncItem and is only counted in SyncReport.Unchanged.
type SyncAction string

// The closed set of sync actions.
const (
	// SyncCopy copies a source object that is missing on the target or
	// differs from the target's object there.
	SyncCopy SyncAction = "copy"
	// SyncDelete deletes a target object with no source object at the
	// same relative key (SyncOptions.Delete only).
	SyncDelete SyncAction = "delete"
)

// SyncActions lists every SyncAction.
var SyncActions = []SyncAction{SyncCopy, SyncDelete}

// SyncItem is one object Sync acted on, or would act on in a dry run.
type SyncItem struct {
	Action SyncAction
	// Key is the object on the source for a copy, on the target for a
	// delete.
	Key string
	// Target is the key on the target disk: where a copy lands, or the
	// object a delete removes (then equal to Key).
	Target string
	// Size is the object's size: the source's for a copy, the target's
	// for a delete.
	Size int64
	// Err is why the action failed; nil when it succeeded and always nil
	// in a dry run.
	Err error
}

// SyncOptions configure Sync. The zero value copies new and changed
// objects and deletes nothing.
type SyncOptions struct {
	// Delete also deletes target objects under the target directory that
	// have no source object at the same relative key, making the target a
	// mirror. Off by default: a sync never deletes what it was not asked
	// to. It never deletes outside the target directory.
	Delete bool
	// DryRun reports every action through OnItem and in the report, and
	// changes nothing on either disk.
	DryRun bool
	// Same decides whether a target object already holds the source
	// object's content; nil means SameContent. It is the escape hatch
	// for backends whose ETags are not comparable but that keep a digest
	// of their own in Metadata.
	Same func(src, dst Entry) bool
	// Checksum reads both objects and compares their SHA-256 when Same
	// says they differ but their sizes are equal: the listings of many
	// backends (a local disk, or two backends whose ETags are computed
	// differently) cannot prove equal content, and without it every such
	// object is copied again on every run. It costs a full read of each
	// such pair; a read failure counts as different, so the object is
	// copied.
	Checksum bool
	// OnItem, when set, receives every action as it is done (or, in a dry
	// run, planned), failures included, in source listing order, then
	// the deletes in target listing order.
	OnItem func(SyncItem)
}

// SyncReport counts what Sync did, or would do in a dry run.
type SyncReport struct {
	Copied    int
	Deleted   int
	Unchanged int
	Failed    int
}

// SameContent reports whether dst holds src's content as far as the
// listings can tell: equal sizes, and equal SHA-256 digests when both
// have one, otherwise equal ETags when both have one. Anything less
// counts as different, so an object whose content cannot be compared is
// copied again rather than wrongly skipped. ETags are compared as
// strings: two backends that compute them differently (an S3 multipart
// ETag and a local one) never match, which costs a copy, never a skip.
func SameContent(src, dst Entry) bool {
	switch {
	case src.Size != dst.Size:
		return false
	case src.SHA256 != "" && dst.SHA256 != "":
		return src.SHA256 == dst.SHA256
	case src.ETag != "" && dst.ETag != "":
		return src.ETag == dst.ETag
	}
	return false
}

// Sync makes the objects under dstDir on dst match those under srcDir on
// src: every source object missing on the target, or whose target object
// differs (SyncOptions.Same), is copied to the same relative key under
// dstDir; with Delete, target objects under dstDir with no source object
// are deleted. Both listings are read in full before anything changes, so
// a listing failure stops Sync with nothing done, and objects it copies
// are never listed back. Each object is handled on its own: a failed copy
// or delete is reported (OnItem, SyncReport.Failed) and the rest go on,
// and the returned error says how many failed with the first cause. A
// rerun after a failure copies only what is still missing or different.
//
// A delete is limited to keys inside dstDir, checked again before every
// deletion, so no listing quirk can make Sync delete outside it. When
// both disks reach the same backend (a scoped disk and the disk it was
// derived from, a routed or mirrored disk and one of its targets), the
// two directories as that backend sees them must not overlap in either
// direction (CodeInvalidArgument): the target would be part of the
// source, and a sync with Delete would remove the source while copying
// it. Delete is refused (CodeInvalidArgument) whenever either side is a
// Route, Overlay, Mirror, CachedRead or Failover disk, or wraps one,
// since such a disk can reach targets no check can see; sync the disks
// underneath it instead.
//
// On a target with the CaseInsensitive capability, source objects whose
// keys differ only in case would overwrite each other there on every
// run: none of them is copied, and each is reported as a failed copy
// with CodeConflict naming the other. Matching target objects is then
// case-insensitive too, so Delete never removes the object a copy wrote.
func Sync(ctx context.Context, src *Disk, srcDir string, dst *Disk, dstDir string, opts SyncOptions) (SyncReport, error) {
	var rep SyncReport
	srcDir, err := CleanDir(srcDir)
	if err != nil {
		return rep, err
	}
	dstDir, err = CleanDir(dstDir)
	if err != nil {
		return rep, err
	}
	if err := checkOverlap(src, srcDir, dst, dstDir, opts.Delete); err != nil {
		return rep, err
	}
	same := opts.Same
	if same == nil {
		same = SameContent
	}
	report := opts.OnItem
	if report == nil {
		report = func(SyncItem) {}
	}
	fold := func(k string) string { return k }
	if dst.Capabilities().CaseInsensitive {
		fold = strings.ToLower
	}
	srcObjects, err := listTree(ctx, src, srcDir, fold)
	if err != nil {
		return rep, err
	}
	dstObjects, err := listTree(ctx, dst, dstDir, fold)
	if err != nil {
		return rep, err
	}
	var first error
	fail := func(it SyncItem) {
		rep.Failed++
		if first == nil {
			first = it.Err
		}
	}
	for _, s := range srcObjects.ordered {
		rel := RelativeKey(s.Key, srcDir)
		target := JoinKey(dstDir, rel)
		if others := srcObjects.collisions(rel, s.Key); len(others) > 0 {
			it := SyncItem{Action: SyncCopy, Key: s.Key, Target: target, Size: s.Size,
				Err: Errorf(CodeConflict, "sync: %q collides with %q on case-insensitive disk %s", s.Key, others[0], dst.Name())}
			report(it)
			fail(it)
			continue
		}
		if d, ok := dstObjects.get(rel); ok && (same(s, d) || opts.Checksum && s.Size == d.Size && sameDigest(ctx, src, s.Key, dst, d.Key)) {
			rep.Unchanged++
			continue
		}
		it := SyncItem{Action: SyncCopy, Key: s.Key, Target: target, Size: s.Size}
		if !opts.DryRun {
			_, it.Err = CopyAcross(ctx, src, s.Key, dst, target)
		}
		report(it)
		if it.Err != nil {
			fail(it)
			continue
		}
		rep.Copied++
	}
	if opts.Delete {
		for _, d := range dstObjects.ordered {
			if _, ok := srcObjects.get(RelativeKey(d.Key, dstDir)); ok {
				continue
			}
			it := SyncItem{Action: SyncDelete, Key: d.Key, Target: d.Key, Size: d.Size}
			switch {
			case !inDir(d.Key, dstDir):
				it.Err = Errorf(CodeInvalidArgument, "sync: refusing to delete %q outside %q", d.Key, dstDir)
			case !opts.DryRun:
				it.Err = dst.Delete(ctx, d.Key)
			}
			report(it)
			if it.Err != nil {
				fail(it)
				continue
			}
			rep.Deleted++
		}
	}
	if first != nil {
		return rep, Errorf(CodeOf(first), "sync: %d objects failed, first: %w", rep.Failed, first)
	}
	return rep, nil
}

// checkOverlap refuses directories that overlap on one backend. Each
// side is resolved to every backend it reaches and the directory that
// backend sees (backendDirs), so a scoped disk and its parent, or a
// routed or mirrored disk and one of its targets, are recognised as one
// backend. A side that reaches more than one target (Route, Overlay,
// Mirror, CachedRead, Failover) is refused for Delete outright: a target
// this code cannot follow (a wrapper from another package) might still
// hold the source, and a sync with Delete would then remove it.
func checkOverlap(src *Disk, srcDir string, dst *Disk, dstDir string, del bool) error {
	srcAt, srcMulti := backendDirs(src.driver, srcDir)
	dstAt, dstMulti := backendDirs(dst.driver, dstDir)
	if del && (srcMulti || dstMulti) {
		return Errorf(CodeInvalidArgument, "sync: refusing Delete between %s:%q and %s:%q: a routed, overlaid, mirrored, cached or failover disk sends keys to several targets, so the target cannot be proven apart from the source; sync the underlying disks instead", src.Name(), srcDir, dst.Name(), dstDir)
	}
	for _, a := range srcAt {
		for _, b := range dstAt {
			if sameBackend(a.drv, b.drv) && (inDir(a.dir, b.dir) || inDir(b.dir, a.dir)) {
				return Errorf(CodeInvalidArgument, "sync: %s:%q and %s:%q overlap on one backend (%q and %q)", src.Name(), srcDir, dst.Name(), dstDir, a.dir, b.dir)
			}
		}
	}
	return nil
}

// backendAt is one backend a directory reaches and the directory there.
type backendAt struct {
	drv Driver
	dir string
}

// maxWrapDepth bounds the walk down wrapper chains, so a cycle of
// wrappers cannot loop forever.
const maxWrapDepth = 64

// backendDirs returns every backend drv sends keys under dir to, with the
// directory each sees, following scoped disks (KeyMapper, Unwrapper) and
// this package's multi-target wrappers, and whether any multi-target
// wrapper was on the way.
func backendDirs(drv Driver, dir string) ([]backendAt, bool) {
	var out []backendAt
	multi := false
	var walk func(drv Driver, dir string, depth int)
	walk = func(drv Driver, dir string, depth int) {
		if depth > maxWrapDepth {
			multi = true
			out = append(out, backendAt{drv, ""})
			return
		}
		switch x := drv.(type) {
		case *routeDriver:
			multi = true
			for _, m := range x.all() {
				switch {
				case dir == m.Prefix:
					walk(m.Disk.driver, "", depth+1)
				case inDir(dir, m.Prefix):
					walk(m.Disk.driver, RelativeKey(dir, m.Prefix), depth+1)
				case inDir(m.Prefix, dir):
					walk(m.Disk.driver, "", depth+1) // mounted below dir: all of it
				}
			}
			return
		case *overlayDriver:
			multi = true
			walk(x.upper.driver, dir, depth+1)
			walk(x.lower.driver, dir, depth+1)
			return
		case *mirrorDriver:
			multi = true
			walk(x.primary, dir, depth+1)
			for _, m := range x.mirrors {
				walk(m.driver, dir, depth+1)
			}
			return
		case *cachedDriver:
			multi = true
			walk(x.scopedDriver, dir, depth+1)
			walk(x.cache.driver, "", depth+1) // the cache's own layout: all of it
			return
		case *failoverDriver:
			multi = true
			walk(x.scopedDriver, dir, depth+1)
			for _, sec := range x.secondaries {
				walk(sec.driver, dir, depth+1)
			}
			return
		}
		if m, ok := drv.(KeyMapper); ok {
			dir = m.InnerKey(dir)
		}
		if u, ok := drv.(Unwrapper); ok {
			walk(u.Unwrap(), dir, depth+1)
			return
		}
		out = append(out, backendAt{drv, dir})
	}
	walk(drv, dir, 0)
	return out, multi
}

// sameBackend reports whether a and b are one driver value. Drivers of a
// type that cannot be compared (a struct holding a slice or a map) are
// taken as the same when their types match: refusing a sync wrongly is
// recoverable, deleting its source is not.
func sameBackend(a, b Driver) bool {
	ta := reflect.TypeOf(a)
	if ta != reflect.TypeOf(b) {
		return false
	}
	if !ta.Comparable() {
		return true
	}
	return a == b
}

// sameDigest reports whether two objects have the same SHA-256, reading
// both; a failure to read either counts as different.
func sameDigest(ctx context.Context, a *Disk, aKey string, b *Disk, bKey string) bool {
	x, err := ObjectSHA256(ctx, a, aKey)
	if err != nil {
		return false
	}
	y, err := ObjectSHA256(ctx, b, bKey)
	return err == nil && x == y
}

// ObjectSHA256 reads the object at key on d in full and returns the hex
// SHA-256 of its content: the digest Disk.Write records in Entry.SHA256,
// computed from the stored bytes rather than trusted from metadata. It is
// for integrity checks (Sync's Checksum, scrubbing records against the
// digest they stored), and costs a full read.
func ObjectSHA256(ctx context.Context, d *Disk, key string) (string, error) {
	rc, err := d.Read(ctx, key)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// tree is one directory's objects in listing order, indexed by key
// relative to the directory, folded (lower-cased on a case-insensitive
// target).
type tree struct {
	fold    func(string) string
	byRel   map[string][]int
	ordered []Entry
}

// get returns the first object at rel (folded).
func (t tree) get(rel string) (Entry, bool) {
	i, ok := t.byRel[t.fold(rel)]
	if !ok {
		return Entry{}, false
	}
	return t.ordered[i[0]], true
}

// collisions returns the keys, other than key, that fold to rel.
func (t tree) collisions(rel, key string) []string {
	var out []string
	for _, i := range t.byRel[t.fold(rel)] {
		if t.ordered[i].Key != key {
			out = append(out, t.ordered[i].Key)
		}
	}
	return out
}

// listTree lists every object under dir on d.
func listTree(ctx context.Context, d *Disk, dir string, fold func(string) string) (tree, error) {
	t := tree{fold: fold, byRel: map[string][]int{}}
	for e, err := range d.List(ctx, dir, ListOptions{Recursive: true}) {
		if err != nil {
			return tree{}, err
		}
		if e.IsDir {
			continue
		}
		rel := fold(RelativeKey(e.Key, dir))
		t.byRel[rel] = append(t.byRel[rel], len(t.ordered))
		t.ordered = append(t.ordered, e)
	}
	return t, nil
}

// inDir reports whether key is dir or below it ("" contains everything).
func inDir(key, dir string) bool {
	return dir == "" || key == dir || strings.HasPrefix(key, dir+"/")
}
