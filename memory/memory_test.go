package memory_test

import (
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/filesystemtest"
	"github.com/ubgo/filesystem/memory"
)

func TestConformance(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		d, err := filesystem.NewDisk("mem", memory.New())
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
}

func TestConformanceWithVersions(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		d, err := filesystem.NewDisk("mem", memory.New(memory.WithVersions()))
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
}

// TestVersionsRecordEveryMutation pins that each way of changing a key
// adds a version or a delete marker when versions are kept.
func TestVersionsRecordEveryMutation(t *testing.T) {
	ctx := t.Context()
	d, err := filesystem.NewDisk("mem", memory.New(memory.WithVersions()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(ctx, "a", []byte("1"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Copy(ctx, "a", "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Move(ctx, "b", "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Append(ctx, "c", strings.NewReader("2"), filesystem.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteKeys(ctx, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string][]bool{"a": {true, false}, "b": {true, false}, "c": {false, false}} {
		vs, err := d.Versions(ctx, key)
		if err != nil || len(vs) != len(want) {
			t.Fatalf("%s: versions %+v, %v", key, vs, err)
		}
		for i, marker := range want {
			if vs[i].DeleteMarker != marker || vs[i].Latest != (i == 0) {
				t.Errorf("%s version %d = %+v", key, i, vs[i])
			}
		}
	}
	if _, err := filesystem.NewDisk("plain", memory.New()); err != nil {
		t.Fatal(err)
	}
	if c := memory.New().Capabilities(); c.Versions || c.VersionRead {
		t.Errorf("versions declared without WithVersions: %v", c)
	}
}
