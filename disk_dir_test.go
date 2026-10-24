package filesystem_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// dirsLiar declares Dirs without implementing DirMaker.
type dirsLiar struct{ *basicDriver }

func (dirsLiar) Capabilities() filesystem.Capabilities { return filesystem.Capabilities{Dirs: true} }

// findWithoutSweep declares FindAbandoned without SweepAbandoned.
type findWithoutSweep struct{ *memory.Driver }

func (findWithoutSweep) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{FindAbandoned: true}
}

// brokenDirs fails every directory call.
type brokenDirs struct{ *memory.Driver }

func (brokenDirs) DirExists(context.Context, string) (bool, error) {
	return true, filesystem.ErrUnavailable
}

func (brokenDirs) FindAbandoned(context.Context, time.Time) ([]filesystem.Abandoned, error) {
	return nil, filesystem.ErrUnavailable
}

func TestDirCapabilitiesAreVerified(t *testing.T) {
	if _, err := filesystem.NewDisk("liar", dirsLiar{newBasic()}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("Dirs declared without the interface: %v", err)
	}
	_, err := filesystem.NewDisk("find", findWithoutSweep{memory.New()})
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || !strings.Contains(err.Error(), "FindAbandoned without SweepAbandoned") {
		t.Errorf("FindAbandoned without SweepAbandoned: %v", err)
	}
	s := memory.New().Capabilities().String()
	if !strings.Contains(s, "find-abandoned") || !strings.Contains(s, "dirs") {
		t.Errorf("String() = %s", s)
	}
}

func TestMakeDirAndDirExists(t *testing.T) {
	ctx := t.Context()
	var seen []filesystem.Op
	rec := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		seen = append(seen, *op)
		return call(ctx)
	}
	d := mustDisk(t, "mem", memory.New(), filesystem.WithInterceptors(rec))
	if err := d.MakeDir(ctx, "/a//b/"); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.DirExists(ctx, "a/b/"); !ok || err != nil {
		t.Errorf("DirExists after MakeDir = %v, %v", ok, err)
	}
	if len(seen) != 2 || seen[0].Kind != filesystem.OpMakeDir || seen[0].Key != "a/b" || !seen[0].Mutating ||
		seen[1].Kind != filesystem.OpDirExists || seen[1].Mutating || !filesystem.OpMakeDir.Mutates() || filesystem.OpDirExists.Mutates() {
		t.Errorf("ops = %+v", seen)
	}
	// The root always exists and is never sent to the driver.
	seen = nil
	if ok, err := d.DirExists(ctx, "/"); !ok || err != nil || len(seen) != 0 {
		t.Errorf("DirExists(root) = %v, %v after %d ops", ok, err, len(seen))
	}
	for _, bad := range []string{"../x", "a\x00b"} {
		wantCode(t, d.MakeDir(ctx, bad), filesystem.CodeInvalidKey)
		_, err := d.DirExists(ctx, bad)
		wantCode(t, err, filesystem.CodeInvalidKey)
	}

	// A read-only disk refuses MakeDir even where it would do nothing.
	for _, drv := range []filesystem.Driver{memory.New(), newBasic()} {
		ro := mustDisk(t, "ro", drv, filesystem.WithInterceptors(filesystem.ReadOnly()))
		wantCode(t, ro.MakeDir(ctx, "x"), filesystem.CodeReadOnly)
	}

	// Without Dirs, MakeDir stores nothing and DirExists lists.
	b := mustDisk(t, "basic", newBasic())
	if err := b.MakeDir(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	if ok, err := b.DirExists(ctx, "p"); ok || err != nil {
		t.Errorf("portable DirExists of an empty MakeDir = %v, %v", ok, err)
	}

	// A failing backend answer is an error, never a true.
	bd := mustDisk(t, "broken", brokenDirs{memory.New()})
	if ok, err := bd.DirExists(ctx, "x"); ok || !errors.Is(err, filesystem.ErrUnavailable) {
		t.Errorf("failing DirExists = %v, %v", ok, err)
	}
}

func TestScopedAndMirroredDirs(t *testing.T) {
	ctx := t.Context()
	base := mustDisk(t, "base", memory.New())
	scoped, err := base.Scope("tenant", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !scoped.Capabilities().Dirs || scoped.Capabilities().FindAbandoned {
		t.Errorf("scoped capabilities %s", scoped.Capabilities())
	}
	if err := scoped.MakeDir(ctx, "docs"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := base.DirExists(ctx, "t1/docs"); !ok {
		t.Error("scoped MakeDir did not add the prefix")
	}
	if ok, _ := scoped.DirExists(ctx, "docs"); !ok {
		t.Error("scoped DirExists did not add the prefix")
	}

	mirror := mustDisk(t, "copy", memory.New())
	objects := mustDisk(t, "objects", newBasic())
	m, err := filesystem.Mirror("mirrored", base, mirror, objects)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Capabilities().Dirs || m.Capabilities().FindAbandoned {
		t.Errorf("mirror capabilities %s", m.Capabilities())
	}
	if err := m.MakeDir(ctx, "m/n"); err != nil {
		t.Fatal(err)
	}
	for _, d := range []*filesystem.Disk{m, base, mirror} {
		if ok, err := d.DirExists(ctx, "m/n"); !ok || err != nil {
			t.Errorf("%s: DirExists after a mirrored MakeDir = %v, %v", d.Name(), ok, err)
		}
	}
	// The primary refusing stops the fan-out.
	if _, err := base.WriteBytes(ctx, "file", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, m.MakeDir(ctx, "file"), filesystem.CodeExists)
	if ok, _ := mirror.DirExists(ctx, "file"); ok {
		t.Error("a refused MakeDir reached the mirrors")
	}
}

func TestWrappersDoNotForwardFindAbandoned(t *testing.T) {
	primary := mustDisk(t, "primary", memory.New())
	second := mustDisk(t, "second", memory.New())
	cache := mustDisk(t, "cache", memory.New())
	f, err := filesystem.Failover("fo", primary, []*filesystem.Disk{second}, filesystem.FailoverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := filesystem.CachedRead("cached", primary, cache, filesystem.CacheOptions{TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	r, err := filesystem.Route("routed", []filesystem.Mount{{Prefix: "a", Disk: primary}})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []*filesystem.Disk{f, c, r} {
		caps := d.Capabilities()
		if caps.FindAbandoned || caps.SweepAbandoned {
			t.Errorf("%s forwards the backend-wide sweep: %s", d.Name(), caps)
		}
		_, err := d.FindAbandoned(t.Context(), time.Now())
		wantCode(t, err, filesystem.CodeUnsupported)
	}
	// Failover and the cached read forward real directories; a route does not.
	if !f.Capabilities().Dirs || !c.Capabilities().Dirs || r.Capabilities().Dirs {
		t.Errorf("Dirs: failover %v, cached %v, route %v", f.Capabilities().Dirs, c.Capabilities().Dirs, r.Capabilities().Dirs)
	}
}

func TestFindAbandoned(t *testing.T) {
	ctx := t.Context()
	_, err := mustDisk(t, "basic", newBasic()).FindAbandoned(ctx, time.Now())
	if !errors.Is(err, filesystem.ErrUnsupported) {
		t.Errorf("without the capability: %v", err)
	}
	// Two uploads started at the same instant and one earlier: oldest
	// first, then by ID.
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := t0
	var seen []filesystem.Op
	rec := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		seen = append(seen, *op)
		return call(ctx)
	}
	d := mustDisk(t, "mem", memory.New(memory.WithClock(func() time.Time { return now })), filesystem.WithInterceptors(rec))
	var ids []string
	for i, at := range []time.Duration{time.Hour, time.Hour, 0} {
		now = t0.Add(at)
		id, err := d.CreateMultipart(ctx, "k"+string(rune('a'+i)), filesystem.WriteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	seen = nil
	found, err := d.FindAbandoned(ctx, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 3 || found[0].ID != ids[2] || found[1].ID != ids[0] || found[2].ID != ids[1] || found[0].Key != "kc" || found[0].Kind != filesystem.AbandonedMultipart {
		t.Errorf("found %+v, want %v in order", found, []string{ids[2], ids[0], ids[1]})
	}
	if len(seen) != 1 || seen[0].Kind != filesystem.OpFindAbandoned || seen[0].Mutating || filesystem.OpFindAbandoned.Mutates() {
		t.Errorf("ops = %+v", seen)
	}
	// A read-only disk still previews: nothing changes.
	ro := mustDisk(t, "ro", memory.New(), filesystem.WithInterceptors(filesystem.ReadOnly()))
	if _, err := ro.FindAbandoned(ctx, time.Now()); err != nil {
		t.Errorf("read-only preview: %v", err)
	}
	if len(filesystem.AbandonedKindValues) != 2 {
		t.Errorf("AbandonedKindValues = %v", filesystem.AbandonedKindValues)
	}

	bd := mustDisk(t, "broken", brokenDirs{memory.New()})
	if found, err := bd.FindAbandoned(ctx, time.Now()); found != nil || !errors.Is(err, filesystem.ErrUnavailable) {
		t.Errorf("failing preview = %v, %v", found, err)
	}
}
