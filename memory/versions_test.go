package memory_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// TestVersionCallsHonourTheContext proves the version calls check the
// context first, like every other call, and change nothing when it ended.
func TestVersionCallsHonourTheContext(t *testing.T) {
	d := memory.New(memory.WithVersions())
	ctx := t.Context()
	if _, err := d.Write(ctx, "k", strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	vs, err := d.Versions(ctx, "k")
	if err != nil || len(vs) != 1 {
		t.Fatalf("versions = %v, %v", vs, err)
	}
	ended, cancel := context.WithCancel(ctx)
	cancel()
	calls := map[string]error{}
	_, calls["Versions"] = d.Versions(ended, "k")
	_, calls["ReadVersion"] = d.ReadVersion(ended, "k", vs[0].ID)
	calls["RestoreVersion"] = d.RestoreVersion(ended, "k", vs[0].ID)
	for name, err := range calls {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s under an ended context: %v", name, err)
		}
	}
	if after, err := d.Versions(ctx, "k"); err != nil || len(after) != 1 {
		t.Fatalf("an ended restore added a version: %v, %v", after, err)
	}
}

// TestVersionsOfUnknownKeysAndIDs proves a key with no history and a
// version ID that was never issued are CodeNotFound, and restoring an
// unknown ID adds no version.
func TestVersionsOfUnknownKeysAndIDs(t *testing.T) {
	d := memory.New(memory.WithVersions())
	ctx := t.Context()
	if _, err := d.Versions(ctx, "never-written"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("versions of an unknown key: %v", err)
	}
	if _, err := d.Write(ctx, "k", strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.RestoreVersion(ctx, "k", "no-such-id"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("restore of an unknown version: %v", err)
	}
	if vs, err := d.Versions(ctx, "k"); err != nil || len(vs) != 1 {
		t.Errorf("a failed restore changed the history: %v, %v", vs, err)
	}
}
