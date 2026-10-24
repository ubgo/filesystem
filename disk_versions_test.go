package filesystem_test

import (
	"context"
	"io"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// overclaiming declares StorageClass and VersionRead over a memory driver
// hidden behind an interface that implements neither.
type overclaiming struct{ filesystem.Driver }

func (o overclaiming) Capabilities() filesystem.Capabilities {
	c := o.Driver.Capabilities()
	c.Versions, c.VersionRead, c.StorageClass = false, false, true
	return c
}

// versionReadOnly implements every version interface but declares
// VersionRead without Versions, where version IDs come from.
type versionReadOnly struct{ *memory.Driver }

func (v versionReadOnly) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{VersionRead: true}
}

func TestStorageClassAndVersionReadWiring(t *testing.T) {
	ctx := t.Context()
	if _, err := filesystem.NewDisk("x", overclaiming{Driver: newBasic()}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("StorageClass declared without SetStorageClass: %v", err)
	}
	if _, err := filesystem.NewDisk("x", versionReadOnly{Driver: memory.New(memory.WithVersions())}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("VersionRead declared without Versions: %v", err)
	}

	var ops []filesystem.Op
	rec := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		err := call(ctx)
		ops = append(ops, *op)
		return err
	}
	d := mustDisk(t, "v", memory.New(memory.WithVersions()), filesystem.WithInterceptors(rec))
	if _, err := d.WriteBytes(ctx, "k", []byte("one"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetStorageClass(ctx, "../k", "COLD"); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("uncleaned key reached the driver: %v", err)
	}
	if err := d.SetStorageClass(ctx, "k", "COLD"); err != nil {
		t.Fatal(err)
	}
	if e, _ := d.Stat(ctx, "k"); e.StorageClass != "COLD" {
		t.Errorf("stat reports class %q", e.StorageClass)
	}
	vs, err := d.Versions(ctx, "k")
	if err != nil || len(vs) != 1 {
		t.Fatalf("versions %v, %v", vs, err)
	}
	rc, err := d.ReadVersion(ctx, "k", vs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "one" {
		t.Errorf("version reads %q", b)
	}
	if err := d.RestoreVersion(ctx, "k", vs[0].ID); err != nil {
		t.Fatal(err)
	}
	want := map[filesystem.OpKind]struct{ mutating, streaming bool }{
		filesystem.OpSetStorageClass: {true, false},
		filesystem.OpReadVersion:     {false, true},
		filesystem.OpRestoreVersion:  {true, false},
	}
	seen := 0
	for _, op := range ops {
		if w, ok := want[op.Kind]; ok {
			seen++
			if op.Mutating != w.mutating || op.Streaming != w.streaming || op.Key != "k" {
				t.Errorf("op %+v", op)
			}
		}
	}
	if seen != len(want) {
		t.Errorf("interceptors saw %d of the new operations", seen)
	}

	// The Disk refuses bad arguments and missing capabilities itself.
	if err := d.SetStorageClass(ctx, "k", ""); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("empty class: %v", err)
	}
	if err := d.RestoreVersion(ctx, "k", ""); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("empty version id: %v", err)
	}
	plain := mustDisk(t, "p", newBasic())
	if err := plain.SetStorageClass(ctx, "k", "COLD"); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("class without the capability: %v", err)
	}
	if _, err := plain.ReadVersion(ctx, "k", "1"); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("version read without the capability: %v", err)
	}
	ro := mustDisk(t, "ro", memory.New(memory.WithVersions()), filesystem.WithInterceptors(filesystem.ReadOnly()))
	if err := ro.RestoreVersion(ctx, "k", "1"); filesystem.CodeOf(err) != filesystem.CodeReadOnly {
		t.Errorf("restore on a read-only disk: %v", err)
	}

	// Wrapper disks forward the new interfaces: a scoped disk keeps both.
	scoped, err := d.Scope("s", "tenant")
	if err != nil {
		t.Fatal(err)
	}
	if c := scoped.Capabilities(); !c.StorageClass || !c.VersionRead {
		t.Errorf("scoped disk declares %v", c)
	}
	if _, err := scoped.WriteBytes(ctx, "a", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := scoped.SetStorageClass(ctx, "a", "COLD"); err != nil {
		t.Fatal(err)
	}
	if e, _ := d.Stat(ctx, "tenant/a"); e.StorageClass != "COLD" {
		t.Errorf("scoped class change landed as %q", e.StorageClass)
	}
}

// TestScopedVersionsStayInScope pins that a scoped disk prefixes the key
// of a version read and a version restore, so neither can reach an object
// outside the scope nor miss the scoped one.
func TestScopedVersionsStayInScope(t *testing.T) {
	ctx := t.Context()
	d := mustDisk(t, "v", memory.New(memory.WithVersions()))
	scoped, err := d.Scope("s", "tenant")
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"one", "two"} {
		if _, err := scoped.WriteBytes(ctx, "k", []byte(body), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	vs, err := d.Versions(ctx, "tenant/k")
	if err != nil || len(vs) != 2 {
		t.Fatalf("versions under the prefix %v, %v", vs, err)
	}
	first := vs[0]
	for _, v := range vs {
		if !v.Latest {
			first = v
		}
	}
	rc, err := scoped.ReadVersion(ctx, "k", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "one" {
		t.Errorf("scoped version read %q, want one", b)
	}
	if err := scoped.RestoreVersion(ctx, "k", first.ID); err != nil {
		t.Fatal(err)
	}
	if b, err := d.ReadAll(ctx, "tenant/k"); err != nil || string(b) != "one" {
		t.Errorf("after a scoped restore the prefixed object reads %q, %v", b, err)
	}
}
