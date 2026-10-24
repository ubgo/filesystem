package filesystem_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// basicDriver exposes only the six Driver methods of a memory driver and
// declares no capabilities, so every Disk fallback path runs against it.
type basicDriver struct{ inner *memory.Driver }

func newBasic() *basicDriver { return &basicDriver{inner: memory.New()} }

func (b *basicDriver) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	return b.inner.Stat(ctx, key)
}
func (b *basicDriver) Read(ctx context.Context, key string, r filesystem.ByteRange) (io.ReadCloser, error) {
	return b.inner.Read(ctx, key, r)
}
func (b *basicDriver) Write(ctx context.Context, key string, body io.Reader, o filesystem.WriteOptions) (filesystem.Entry, error) {
	return b.inner.Write(ctx, key, body, o)
}
func (b *basicDriver) Delete(ctx context.Context, key string) error { return b.inner.Delete(ctx, key) }
func (b *basicDriver) List(ctx context.Context, dir string, o filesystem.ListOptions) (filesystem.Page, error) {
	return b.inner.List(ctx, dir, o)
}
func (b *basicDriver) Capabilities() filesystem.Capabilities { return filesystem.Capabilities{} }

// fullDriver is a memory driver that also presigns, serves public URLs and
// presigns parts, with deterministic fake URLs, to exercise those paths.
type fullDriver struct{ *memory.Driver }

func (f fullDriver) Capabilities() filesystem.Capabilities {
	c := f.Driver.Capabilities()
	c.Presign, c.PublicURL, c.PresignPart = true, true, true
	return c
}
func (f fullDriver) Presign(_ context.Context, key string, o filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	return filesystem.SignedRequest{Method: o.Method, URL: "https://backend.test/" + key + "?sig=1", ExpiresAt: fixedNow.Add(o.Expires)}, nil
}
func (f fullDriver) PublicURL(_ context.Context, key string) (string, error) {
	return "https://bucket.test/" + key, nil
}
func (f fullDriver) PresignPart(_ context.Context, key, id string, n int, exp time.Duration) (filesystem.SignedRequest, error) {
	return filesystem.SignedRequest{Method: filesystem.MethodPut, URL: "https://backend.test/" + key + "?part", ExpiresAt: fixedNow.Add(exp)}, nil
}
func (f fullDriver) Native() *memory.Driver { return f.Driver }

var fixedNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func mustDisk(t *testing.T, name string, d filesystem.Driver, opts ...filesystem.Option) *filesystem.Disk {
	t.Helper()
	disk, err := filesystem.NewDisk(name, d, opts...)
	if err != nil {
		t.Fatalf("NewDisk: %v", err)
	}
	return disk
}

func wantCode(t *testing.T, err error, code filesystem.Code) {
	t.Helper()
	if got := filesystem.CodeOf(err); got != code {
		t.Fatalf("error code = %q (err %v), want %q", got, err, code)
	}
}
