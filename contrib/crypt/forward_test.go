package crypt_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/crypt"
	"github.com/ubgo/filesystem/memory"
)

// holdingDriver is a memory driver that also declares Hold and Restore
// (which memory lacks) and records what reaches it, so the test can see
// the crypt driver forward those calls with the key unchanged.
type holdingDriver struct {
	*memory.Driver
	hold     filesystem.Hold
	keys     []string
	restored filesystem.RestoreOptions
}

func (h *holdingDriver) Capabilities() filesystem.Capabilities {
	c := h.Driver.Capabilities()
	c.Hold, c.Restore = true, true
	return c
}

func (h *holdingDriver) Hold(_ context.Context, key string) (filesystem.Hold, error) {
	h.keys = append(h.keys, key)
	return h.hold, nil
}

func (h *holdingDriver) SetHold(_ context.Context, key string, hold filesystem.Hold) error {
	h.keys = append(h.keys, key)
	h.hold = hold
	return nil
}

func (h *holdingDriver) Restore(_ context.Context, key string, opts filesystem.RestoreOptions) error {
	h.keys = append(h.keys, key)
	h.restored = opts
	return nil
}

func cryptOver(t *testing.T, inner filesystem.Driver) *crypt.Driver {
	t.Helper()
	kms := crypt.NewMemoryKMS()
	if err := kms.Add("k1", masterKey); err != nil {
		t.Fatal(err)
	}
	drv, err := crypt.New(inner, crypt.NewKMSKeyring(kms, func(context.Context, string) (string, error) { return "k1", nil }))
	if err != nil {
		t.Fatal(err)
	}
	return drv
}

// TestForwardsHoldAndRestore pins that holds and archive restores, which do
// not depend on the stored bytes, pass through the crypt driver to the
// wrapped one, and that the capabilities follow the wrapped driver's.
func TestForwardsHoldAndRestore(t *testing.T) {
	inner := &holdingDriver{Driver: memory.New()}
	drv := cryptOver(t, inner)
	if c := drv.Capabilities(); !c.Hold || !c.Restore {
		t.Fatalf("capabilities not forwarded: Hold %v, Restore %v", c.Hold, c.Restore)
	}
	disk, err := filesystem.NewDisk("enc", drv)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	want := filesystem.Hold{Legal: true, RetainUntil: time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)}
	if err := disk.SetHold(ctx, "a.txt", want); err != nil {
		t.Fatal(err)
	}
	if got, err := disk.Hold(ctx, "a.txt"); err != nil || got != want {
		t.Fatalf("Hold = %+v, %v; want %+v", got, err, want)
	}
	const days = 3
	if err := disk.Restore(ctx, "a.txt", filesystem.RestoreOptions{Days: days}); err != nil {
		t.Fatal(err)
	}
	if inner.restored.Days != days {
		t.Fatalf("restore reached the inner driver with %+v", inner.restored)
	}
	if len(inner.keys) != 3 || inner.keys[0] != "a.txt" || inner.keys[1] != "a.txt" || inner.keys[2] != "a.txt" {
		t.Fatalf("keys reaching the inner driver: %q", inner.keys)
	}
}

// TestForwardWithoutCapability pins the guard for a caller that skips
// Capabilities and calls the driver directly: a wrapped driver without
// the interface answers CodeUnsupported rather than panicking.
func TestForwardWithoutCapability(t *testing.T) {
	drv := cryptOver(t, memory.New())
	if c := drv.Capabilities(); c.Hold || c.Restore {
		t.Fatalf("memory declares Hold %v, Restore %v through crypt", c.Hold, c.Restore)
	}
	ctx := t.Context()
	if _, err := drv.Hold(ctx, "a.txt"); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("Hold: %v, want CodeUnsupported", err)
	}
	if err := drv.SetHold(ctx, "a.txt", filesystem.Hold{Legal: true}); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("SetHold: %v, want CodeUnsupported", err)
	}
	if err := drv.Restore(ctx, "a.txt", filesystem.RestoreOptions{Days: 1}); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("Restore: %v, want CodeUnsupported", err)
	}
}

// TestForwardsDirsAndPreview pins that directories and the sweep preview,
// which hold no bytes, pass through to the wrapped driver, and that a
// wrapped driver without them answers CodeUnsupported.
func TestForwardsDirsAndPreview(t *testing.T) {
	ctx := t.Context()
	inner := memory.New()
	drv := cryptOver(t, inner)
	if c := drv.Capabilities(); !c.Dirs || !c.FindAbandoned {
		t.Fatalf("capabilities not forwarded: Dirs %v, FindAbandoned %v", c.Dirs, c.FindAbandoned)
	}
	disk, err := filesystem.NewDisk("enc", drv)
	if err != nil {
		t.Fatal(err)
	}
	if err := disk.MakeDir(ctx, "docs"); err != nil {
		t.Fatal(err)
	}
	if ok, err := inner.DirExists(ctx, "docs"); !ok || err != nil {
		t.Fatalf("MakeDir did not reach the wrapped driver: %v, %v", ok, err)
	}
	if ok, err := disk.DirExists(ctx, "docs"); !ok || err != nil {
		t.Fatalf("DirExists = %v, %v", ok, err)
	}
	id, err := inner.CreateMultipart(ctx, "big.bin", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	found, err := disk.FindAbandoned(ctx, time.Now().Add(time.Hour))
	if err != nil || len(found) != 1 || found[0].ID != id || found[0].Key != "big.bin" {
		t.Fatalf("FindAbandoned = %+v, %v", found, err)
	}

	bare := cryptOver(t, bareDriver{memory.New()})
	if _, err := bare.FindAbandoned(ctx, time.Now()); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("FindAbandoned: %v, want CodeUnsupported", err)
	}
	if err := bare.MakeDir(ctx, "d"); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("MakeDir: %v, want CodeUnsupported", err)
	}
	if _, err := bare.DirExists(ctx, "d"); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("DirExists: %v, want CodeUnsupported", err)
	}
}

// bareDriver hides every optional interface of the memory driver: only
// the six Driver methods are reachable through it.
type bareDriver struct{ inner *memory.Driver }

func (b bareDriver) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	return b.inner.Stat(ctx, key)
}

func (b bareDriver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	return b.inner.Read(ctx, key, rng)
}

func (b bareDriver) Write(ctx context.Context, key string, body io.Reader, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	return b.inner.Write(ctx, key, body, opts)
}

func (b bareDriver) Delete(ctx context.Context, key string) error { return b.inner.Delete(ctx, key) }

func (b bareDriver) List(ctx context.Context, dir string, opts filesystem.ListOptions) (filesystem.Page, error) {
	return b.inner.List(ctx, dir, opts)
}

func (bareDriver) Capabilities() filesystem.Capabilities { return filesystem.Capabilities{} }
