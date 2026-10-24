package main

import (
	"bytes"
	"context"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/filesystemtest"
	"github.com/ubgo/filesystem/layer"
)

// TestConformance runs the suite every built-in driver passes. Each case
// gets a fresh, empty disk; cases for capabilities the driver does not
// declare are skipped, and the declared ones must hold.
// ds:def id=testconformance-7wqaf78d
func TestConformance(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		d, err := filesystem.NewDisk("kv", New(NewMapBackend()))
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
}

func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"wrote docs/readme.txt (27 bytes, text/plain; charset=utf-8", "listed [docs/copy.txt docs/readme.txt]", "capabilities [metadata,content-type,conditional-write]", "native client"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// overclaiming declares server-side copy without implementing Copier.
type overclaiming struct{ *Driver }

func (o overclaiming) Capabilities() filesystem.Capabilities {
	c := o.Driver.Capabilities()
	c.Copy = true
	return c
}

// ds:def id=testnewdiskrefusesanoverclaimingdriver-773vjepn
func TestNewDiskRefusesAnOverclaimingDriver(t *testing.T) {
	_, err := filesystem.NewDisk("kv", overclaiming{New(NewMapBackend())})
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || !strings.Contains(err.Error(), "declares capability Copy") {
		t.Errorf("err = %v", err)
	}
}

// throttled fails its first Get with the service's throttling error.
type throttled struct {
	Backend
	calls atomic.Int32
}

func (b *throttled) Get(ctx context.Context, key string) (Object, error) {
	if b.calls.Add(1) == 1 {
		return Object{}, ErrThrottled
	}
	return b.Backend.Get(ctx, key)
}

func TestMappedErrorsDriveRetry(t *testing.T) {
	b := &throttled{Backend: NewMapBackend()}
	d, err := filesystem.NewDisk("kv", New(b), filesystem.WithInterceptors(layer.Retry(layer.RetryOptions{Sleep: func(context.Context, time.Duration) error { return nil }})))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Stat(t.Context(), "missing"); filesystem.CodeOf(err) != filesystem.CodeNotFound || b.calls.Load() != 2 {
		t.Errorf("err = %v after %d calls", err, b.calls.Load())
	}
	b.Backend = denying{}
	if _, err := d.Stat(t.Context(), "k"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Errorf("forbidden = %v", err)
	}
	if _, err := Opener(t.Context(), mustURL(t, "kv://x?region=eu")); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("unknown parameter accepted: %v", err)
	}
}

// denying refuses everything the way a backend without credentials does.
type denying struct{ Backend }

func (denying) Get(context.Context, string) (Object, error) { return Object{}, ErrForbidden }

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
