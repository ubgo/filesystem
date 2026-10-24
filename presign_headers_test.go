package filesystem_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

var pinned = filesystem.HTTPHeaders{CacheControl: "public, max-age=60", ContentDisposition: "attachment; filename=a.png", ContentEncoding: "gzip"}

// headerPresigner is a memory driver that presigns and records the options,
// pinning headers when pins is set.
type headerPresigner struct {
	*memory.Driver
	pins bool
	last *filesystem.PresignOptions
}

func (h headerPresigner) Capabilities() filesystem.Capabilities {
	c := h.Driver.Capabilities()
	c.Presign, c.PresignHeaders = true, h.pins
	return c
}

func (h headerPresigner) Presign(_ context.Context, key string, o filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	*h.last = o
	return filesystem.SignedRequest{Method: o.Method, URL: "https://backend.test/" + key}, nil
}

// pinningSigner is recordingSigner reporting whether it pins headers.
type pinningSigner struct {
	recordingSigner
	pins bool
}

func (p pinningSigner) PinsHeaders() bool { return p.pins }

// headerless is a memory driver that stores no headers.
type headerless struct{ *memory.Driver }

func (h headerless) Capabilities() filesystem.Capabilities {
	c := h.Driver.Capabilities()
	c.Headers = false
	return c
}

func TestPresignedPutHeaders(t *testing.T) {
	ctx := t.Context()
	put := filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, Headers: pinned}
	var last filesystem.PresignOptions

	// Driver signing: only a driver that declares PresignHeaders gets them.
	d := mustDisk(t, "drv", headerPresigner{Driver: memory.New(), pins: true, last: &last})
	if _, err := d.SignedURL(ctx, "a.png", put); err != nil || last.Headers != pinned {
		t.Errorf("pinning driver: %+v, %v", last.Headers, err)
	}
	d = mustDisk(t, "drv", headerPresigner{Driver: memory.New(), last: &last})
	if _, err := d.SignedURL(ctx, "a.png", put); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("non-pinning driver: %v", err)
	}

	// Signer signing: the driver must store headers and the signer pin them.
	for _, c := range []struct {
		name   string
		driver filesystem.Driver
		signer filesystem.URLSigner
		want   filesystem.Code
	}{
		{"pinning signer", memory.New(), pinningSigner{recordingSigner{&last}, true}, ""},
		{"signer that says no", memory.New(), pinningSigner{recordingSigner{&last}, false}, filesystem.CodeUnsupported},
		{"signer unaware of headers", memory.New(), recordingSigner{&last}, filesystem.CodeUnsupported},
		{"driver without headers", headerless{memory.New()}, pinningSigner{recordingSigner{&last}, true}, filesystem.CodeUnsupported},
	} {
		last = filesystem.PresignOptions{}
		disk := mustDisk(t, "s", c.driver, filesystem.WithURLSigner(c.signer))
		_, err := disk.SignedURL(ctx, "a.png", put)
		if filesystem.CodeOf(err) != c.want || (c.want == "" && last.Headers != pinned) {
			t.Errorf("%s: %v, signed %+v", c.name, err, last.Headers)
		}
	}

	// Headers belong to a PUT and must not split a header line.
	disk := mustDisk(t, "s", memory.New(), filesystem.WithURLSigner(pinningSigner{recordingSigner{&last}, true}))
	get := put
	get.Method = filesystem.MethodGet
	bad := put
	bad.Headers.CacheControl = "x\r\nSet-Cookie: a=b"
	for _, o := range []filesystem.PresignOptions{get, bad} {
		if _, err := disk.SignedURL(ctx, "a.png", o); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s %+v: %v", o.Method, o.Headers, err)
		}
	}
}

func TestPresignHeadersCapabilityIsVerified(t *testing.T) {
	var last filesystem.PresignOptions
	// PresignHeaders needs Presign and Headers.
	for _, d := range []filesystem.Driver{
		claims{memory.New(), filesystem.Capabilities{PresignHeaders: true, Headers: true}},
		claims{memory.New(), filesystem.Capabilities{PresignHeaders: true, Presign: true}},
	} {
		if _, err := filesystem.NewDisk("x", d); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%+v accepted: %v", d.Capabilities(), err)
		}
	}
	d := mustDisk(t, "ok", headerPresigner{Driver: memory.New(), pins: true, last: &last})
	if !strings.Contains(d.Capabilities().String(), "presign-headers") {
		t.Errorf("caps %v", d.Capabilities())
	}
}

// claims is a memory driver declaring exactly caps.
type claims struct {
	*memory.Driver
	caps filesystem.Capabilities
}

func (c claims) Capabilities() filesystem.Capabilities { return c.caps }

func (claims) Presign(context.Context, string, filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	return filesystem.SignedRequest{}, nil
}

func TestHTTPHeadersOnTheWire(t *testing.T) {
	h := http.Header{"X-Other": {"kept"}}
	pinned.SetOn(h)
	if h.Get(filesystem.HeaderCacheControl) != pinned.CacheControl || h.Get(filesystem.HeaderContentDisposition) != pinned.ContentDisposition || h.Get(filesystem.HeaderContentEncoding) != "gzip" || h.Get("X-Other") != "kept" {
		t.Errorf("SetOn = %v", h)
	}
	if got := filesystem.HTTPHeadersFrom(h); got != pinned {
		t.Errorf("round trip = %+v", got)
	}
	empty := http.Header{}
	filesystem.HTTPHeaders{CacheControl: "no-store"}.SetOn(empty)
	if len(empty) != 1 {
		t.Errorf("empty fields were set: %v", empty)
	}
}

func TestMirrorDoesNotClaimPresignHeaders(t *testing.T) {
	var last filesystem.PresignOptions
	primary := mustDisk(t, "p", headerPresigner{Driver: memory.New(), pins: true, last: &last})
	m, err := filesystem.Mirror("m", primary, mustDisk(t, "copy", memory.New()))
	if err != nil {
		t.Fatal(err)
	}
	if m.Capabilities().PresignHeaders {
		t.Error("mirror claims PresignHeaders although it refuses signed PUTs")
	}
}
