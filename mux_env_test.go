package filesystem_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// closing is a memory driver that counts Close calls.
type closing struct {
	*memory.Driver
	closed *int
}

func (c closing) Close() error { *c.closed++; return nil }

func envMux(t *testing.T, closed *int) *filesystem.Mux {
	t.Helper()
	m := filesystem.NewMux()
	if err := m.Register("mem", func(_ context.Context, u *url.URL) (filesystem.Driver, error) {
		if err := filesystem.RejectParams(u); err != nil {
			return nil, err
		}
		return closing{memory.New(), closed}, nil
	}); err != nil {
		t.Fatal(err)
	}
	return m
}

func lookup(env map[string]string) filesystem.LookupFunc {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

func TestOpenManager(t *testing.T) {
	ctx := t.Context()
	var closed int
	m := envMux(t, &closed)
	mgr, err := m.OpenManager(ctx, "STORAGE_", lookup(map[string]string{
		"STORAGE_DISKS":         " media , media-cdn ",
		"STORAGE_MEDIA_DSN":     "mem://x?visibility=public",
		"STORAGE_MEDIA_CDN_DSN": "mem://y",
		"STORAGE_DEFAULT_DISK":  "media-cdn",
	}), filesystem.WithDefaultVisibility(filesystem.VisibilityPrivate))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(mgr.Names(), ",") != "media,media-cdn" {
		t.Fatalf("names = %v", mgr.Names())
	}
	if d, _ := mgr.Default(); d.Name() != "media-cdn" {
		t.Fatalf("default = %s", d.Name())
	}
	if filesystem.EnvName("STORAGE_", "media-cdn.v2") != "STORAGE_MEDIA_CDN_V2_DSN" {
		t.Fatal("EnvName")
	}

	// Without DEFAULT_DISK the first listed is the default.
	mgr, err = m.OpenManager(ctx, "", lookup(map[string]string{"DISKS": "b,a", "B_DSN": "mem://b", "A_DSN": "mem://a"}))
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := mgr.Default(); d.Name() != "b" {
		t.Fatalf("default = %s", d.Name())
	}
}

func TestOpenManagerRefusals(t *testing.T) {
	ctx := t.Context()
	const secret = "s3cr3t-password"
	for name, c := range map[string]struct {
		env  map[string]string
		want string // the variable the error must name
	}{
		"no disks":          {map[string]string{}, "DISKS"},
		"empty list":        {map[string]string{"DISKS": " , "}, "DISKS"},
		"missing DSN":       {map[string]string{"DISKS": "a,b", "A_DSN": "mem://a"}, "B_DSN"},
		"blank DSN":         {map[string]string{"DISKS": "a,b", "A_DSN": "mem://a", "B_DSN": "  "}, "B_DSN"},
		"same variable":     {map[string]string{"DISKS": "a-b,a.b", "A_B_DSN": "mem://x"}, "A_B_DSN"},
		"unknown scheme":    {map[string]string{"DISKS": "a,b", "A_DSN": "mem://a", "B_DSN": "ftp://user:" + secret + "@host"}, "disk b"},
		"default not there": {map[string]string{"DISKS": "a", "A_DSN": "mem://a", "DEFAULT_DISK": "z"}, "DEFAULT_DISK"},
	} {
		t.Run(name, func(t *testing.T) {
			var closed int
			_, err := envMux(t, &closed).OpenManager(ctx, "", lookup(c.env))
			if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), secret) {
				t.Fatalf("err = %v, want one naming %s and no secret", err, c.want)
			}
			opened := strings.Count(c.env["DISKS"], ",") // disks opened before the failing one
			if name == "missing DSN" || name == "blank DSN" || name == "unknown scheme" {
				if closed != opened {
					t.Fatalf("closed %d of the %d disks opened", closed, opened)
				}
			}
		})
	}
	if _, err := filesystem.NewMux().OpenManager(ctx, "", nil); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("nil lookup = %v", err)
	}
}

// TestOpenManagerErrorsNameVariableAndDisk pins that an error from opening
// one disk (the opener's, the Mux's own parameters, or the Disk's) names
// the variable and the disk, keeps its code, never shows the DSN, and
// leaves no driver open.
func TestOpenManagerErrorsNameVariableAndDisk(t *testing.T) {
	ctx := t.Context()
	const secret = "s3cr3t-password"
	for name, c := range map[string]struct {
		dsn    string
		code   filesystem.Code
		closed int // disk a's driver, plus the refused disk's when it was built
	}{
		"opener refuses a parameter": {"mem://" + secret + "?bogus=1", filesystem.CodeInvalidArgument, 1},
		"bad Mux parameter":          {"mem://" + secret + "?readonly=maybe", filesystem.CodeInvalidArgument, 1},
		"bad prefix":                 {"mem://" + secret + "?prefix=../up", filesystem.CodeInvalidKey, 1},
		"Disk refuses an option":     {"mem://" + secret + "?visibility=bogus", filesystem.CodeInvalidArgument, 2},
		"Disk refuses with a prefix": {"mem://" + secret + "?visibility=bogus&prefix=p", filesystem.CodeInvalidArgument, 2},
	} {
		t.Run(name, func(t *testing.T) {
			var closed int
			_, err := envMux(t, &closed).OpenManager(ctx, "STORAGE_", lookup(map[string]string{
				"STORAGE_DISKS": "a,media", "STORAGE_A_DSN": "mem://a", "STORAGE_MEDIA_DSN": c.dsn,
			}))
			msg := fmt.Sprint(err)
			if filesystem.CodeOf(err) != c.code || !strings.Contains(msg, "STORAGE_MEDIA_DSN: disk media: ") || strings.Contains(msg, secret) {
				t.Fatalf("err = %v, want %s naming STORAGE_MEDIA_DSN and disk media, without the DSN", err, c.code)
			}
			if closed != c.closed {
				t.Fatalf("closed %d drivers, want %d", closed, c.closed)
			}
		})
	}
}

func TestMuxOpenClosesWhatItBuilt(t *testing.T) {
	ctx := t.Context()
	var closed int
	m := envMux(t, &closed)
	// A driver built for a Disk that cannot be made is closed.
	if _, err := m.Open(ctx, "d", "mem://x?visibility=bogus"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || !strings.Contains(err.Error(), "disk d: ") {
		t.Fatalf("err = %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d after a refused disk", closed)
	}
	// Closing a prefix-scoped disk from a DSN closes its driver.
	d, err := m.Open(ctx, "d", "mem://x?prefix=tenant")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil || closed != 2 {
		t.Fatalf("close = %v, closed = %d", err, closed)
	}
	// One without resources closes cleanly.
	plain := filesystem.NewMux()
	_ = plain.Register("mem", func(context.Context, *url.URL) (filesystem.Driver, error) { return memory.New(), nil })
	d, err = plain.Open(ctx, "d", "mem://x?prefix=tenant")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	// An opener error without a cause still names the disk.
	_ = plain.Register("deny", func(context.Context, *url.URL) (filesystem.Driver, error) {
		return nil, filesystem.NewError(filesystem.CodePermission, nil)
	})
	if _, err := plain.Open(ctx, "d", "deny://x"); filesystem.CodeOf(err) != filesystem.CodePermission || err.Error() != "filesystem: permission: disk d" {
		t.Fatalf("causeless opener error = %v", err)
	}
}
