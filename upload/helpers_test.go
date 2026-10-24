package upload_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// Sample file heads whose types sniff detects.
const (
	pngData  = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-png-body"
	htmlData = "<!DOCTYPE html><script>alert(1)</script>"
	svgData  = `<svg xmlns="http://www.w3.org/2000/svg"><script>x</script></svg>`
)

var images = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}}

func newDisk(t *testing.T, d filesystem.Driver, opts ...filesystem.Option) *filesystem.Disk {
	t.Helper()
	disk, err := filesystem.NewDisk("uploads", d, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return disk
}

func wantCode(t *testing.T, err error, code filesystem.Code) {
	t.Helper()
	if got := filesystem.CodeOf(err); got != code {
		t.Fatalf("code %q (err %v), want %q", got, err, code)
	}
}

// presigning is a memory driver that also signs (fake) URLs and parts, the
// way an S3 driver would.
type presigning struct{ *memory.Driver }

func (p presigning) Capabilities() filesystem.Capabilities {
	c := p.Driver.Capabilities()
	c.Presign, c.PresignPart, c.PublicURL = true, true, true
	return c
}
func (presigning) Presign(_ context.Context, key string, o filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	h := map[string][]string{}
	if o.ContentType != "" {
		h["Content-Type"] = []string{o.ContentType}
	}
	return filesystem.SignedRequest{Method: o.Method, URL: "https://bucket.test/" + key + "?sig", Header: h, ExpiresAt: time.Unix(0, 0).Add(o.Expires)}, nil
}
func (presigning) PresignPart(_ context.Context, key, id string, n int, _ time.Duration) (filesystem.SignedRequest, error) {
	return filesystem.SignedRequest{Method: filesystem.MethodPut, URL: "https://bucket.test/" + key + "?part"}, nil
}
func (presigning) PublicURL(_ context.Context, key string) (string, error) {
	return "https://cdn.test/" + key, nil
}

// undeletable fails Delete, to cover cleanup failures.
type undeletable struct{ *memory.Driver }

func (undeletable) Delete(context.Context, string) error { return filesystem.ErrPermission }

// unlistable fails List.
type unlistable struct{ *memory.Driver }

func (unlistable) List(context.Context, string, filesystem.ListOptions) (filesystem.Page, error) {
	return filesystem.Page{}, filesystem.ErrUnavailable
}

// unreadable fails Read.
type unreadable struct{ *memory.Driver }

func (unreadable) Read(context.Context, string, filesystem.ByteRange) (io.ReadCloser, error) {
	return nil, filesystem.ErrUnavailable
}

// brokenStream fails mid-read.
type brokenStream struct{ *memory.Driver }

func (brokenStream) Read(context.Context, string, filesystem.ByteRange) (io.ReadCloser, error) {
	return io.NopCloser(&failing{}), nil
}

type failing struct{}

func (*failing) Read([]byte) (int, error) { return 0, filesystem.ErrUnavailable }

func reader(s string) io.Reader { return strings.NewReader(s) }
