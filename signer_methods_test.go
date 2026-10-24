package filesystem_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// Hosts the two signing paths answer from, so each case can tell which one
// signed.
const (
	signerHost = "https://app.test/"
	driverHost = "https://backend.test/"
)

var putOpts = filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute}

// signedBy reports which path signed a URL: the URLSigner or the driver.
func signedBy(t *testing.T, d *filesystem.Disk, o filesystem.PresignOptions) string {
	t.Helper()
	r, err := d.SignedURL(t.Context(), "k", o)
	if err != nil {
		t.Fatalf("SignedURL(%s): %v", o.Method, err)
	}
	switch {
	case strings.HasPrefix(r.URL, signerHost):
		return "signer"
	case strings.HasPrefix(r.URL, driverHost):
		return "driver"
	}
	t.Fatalf("SignedURL(%s) = %q, from neither path", o.Method, r.URL)
	return ""
}

// TestSignerRoutesByMethod pins which path signs each method: a CDN signer
// set on a presigning disk (S3, GCS, Azure) signs downloads while uploads
// stay with the driver, an explicit method list replaces that rule, and a
// disk that cannot presign keeps using the signer for both, as before.
func TestSignerRoutesByMethod(t *testing.T) {
	cases := []struct {
		name     string
		driver   filesystem.Driver
		opts     []filesystem.Option
		get, put string // "signer", "driver" or "" for CodeUnsupported
	}{
		{"presigning driver, no signer", fullDriver{memory.New()}, nil, "driver", "driver"},
		{"presigning driver, default signer", fullDriver{memory.New()}, []filesystem.Option{filesystem.WithURLSigner(stubSigner{})}, "signer", "driver"},
		{"presigning driver, signer for both", fullDriver{memory.New()}, []filesystem.Option{filesystem.WithURLSigner(stubSigner{}, filesystem.MethodGet, filesystem.MethodPut)}, "signer", "signer"},
		{"presigning driver, signer for uploads", fullDriver{memory.New()}, []filesystem.Option{filesystem.WithURLSigner(stubSigner{}, filesystem.MethodPut)}, "driver", "signer"},
		{"no presign, default signer", memory.New(), []filesystem.Option{filesystem.WithURLSigner(stubSigner{})}, "signer", "signer"},
		{"no presign, signer for downloads only", memory.New(), []filesystem.Option{filesystem.WithURLSigner(stubSigner{}, filesystem.MethodGet)}, "signer", ""},
		{"no presign, no signer", memory.New(), nil, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := mustDisk(t, "d", c.driver, c.opts...)
			for _, m := range []struct {
				opts filesystem.PresignOptions
				want string
			}{{getOpts, c.get}, {putOpts, c.put}} {
				if m.want == "" {
					_, err := d.SignedURL(t.Context(), "k", m.opts)
					wantCode(t, err, filesystem.CodeUnsupported)
					continue
				}
				if got := signedBy(t, d, m.opts); got != m.want {
					t.Errorf("%s signed by %s, want %s", m.opts.Method, got, m.want)
				}
			}
		})
	}
}

// TestSignerFailsClosed pins that a method routed to the signer never falls
// back to the driver: a signer error, or a signer that cannot enforce
// AllowedIP while the driver could, fails the call instead of handing out
// a raw bucket link.
func TestSignerFailsClosed(t *testing.T) {
	ctx := t.Context()
	failing := mustDisk(t, "d", fullDriver{memory.New()}, filesystem.WithURLSigner(failingSigner{}))
	r, err := failing.SignedURL(ctx, "k", getOpts)
	wantCode(t, err, filesystem.CodePermission)
	if r.URL != "" {
		t.Errorf("a failed signer still returned %q", r.URL)
	}
	// Uploads were never the signer's, so they still presign.
	if got := signedBy(t, failing, putOpts); got != "driver" {
		t.Errorf("PUT signed by %s, want driver", got)
	}

	var last filesystem.PresignOptions
	ip := getOpts
	ip.AllowedIP = "203.0.113.0/24"
	plain := mustDisk(t, "d", ipPresigner{Driver: memory.New(), restricts: true, last: &last}, filesystem.WithURLSigner(restrictingSigner{recordingSigner: recordingSigner{last: &last}}))
	_, err = plain.SignedURL(ctx, "k", ip)
	wantCode(t, err, filesystem.CodeUnsupported)
}

// TestWithURLSignerRefusesUnknownMethod pins that a method list naming
// anything but GET or PUT is a configuration error at NewDisk.
func TestWithURLSignerRefusesUnknownMethod(t *testing.T) {
	_, err := filesystem.NewDisk("d", memory.New(), filesystem.WithURLSigner(stubSigner{}, "DELETE"))
	wantCode(t, err, filesystem.CodeInvalidArgument)
}
