package filesystem_test

import (
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// TestDriverKey: DriverKey cleans the key and applies every scope on the
// way down, so a key-bound backend option names the key the driver gets.
func TestDriverKey(t *testing.T) {
	d, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := d.Scope("tenant", "t1")
	if err != nil {
		t.Fatal(err)
	}
	inner, err := tenant.Scope("avatars", "img")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		disk      *filesystem.Disk
		key, want string
	}{
		{d, "a.png", "a.png"},
		{tenant, "a.png", "t1/a.png"},
		{inner, "/x//a.png", "t1/img/x/a.png"},
	} {
		got, err := c.disk.DriverKey(c.key)
		if err != nil || got != c.want {
			t.Errorf("%s.DriverKey(%q) = %q, %v; want %q", c.disk.Name(), c.key, got, err, c.want)
		}
	}
	if _, err := tenant.DriverKey("../escape"); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("traversal = %v, want invalid_key", err)
	}
}
