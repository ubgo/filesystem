package filesystem_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// ipPresigner is a memory driver that presigns, declares PresignIP when
// restricts is set, and records the options it was given.
type ipPresigner struct {
	*memory.Driver
	restricts bool
	last      *filesystem.PresignOptions
}

func (p ipPresigner) Capabilities() filesystem.Capabilities {
	c := p.Driver.Capabilities()
	c.Presign, c.PresignIP = true, p.restricts
	return c
}

func (p ipPresigner) Presign(_ context.Context, key string, o filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	*p.last = o
	return filesystem.SignedRequest{Method: o.Method, URL: "https://backend.test/" + key}, nil
}

// restrictingSigner is recordingSigner reporting whether it enforces
// AllowedIP.
type restrictingSigner struct {
	recordingSigner
	restricts bool
}

func (r restrictingSigner) RestrictsIP() bool { return r.restricts }

// TestPresignAllowedIP proves an IP-restricted URL is issued only where the
// restriction is enforced (a driver declaring PresignIP, a signer reporting
// RestrictsIP), is CodeUnsupported everywhere else, and that a malformed or
// non-canonical prefix is refused before any signing.
func TestPresignAllowedIP(t *testing.T) {
	ctx := t.Context()
	get := filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute, AllowedIP: "203.0.113.0/24"}
	var last filesystem.PresignOptions
	cases := []struct {
		name string
		disk *filesystem.Disk
		ok   bool
	}{
		{"restricting driver", mustDisk(t, "a", ipPresigner{Driver: memory.New(), restricts: true, last: &last}), true},
		{"plain driver", mustDisk(t, "b", ipPresigner{Driver: memory.New(), last: &last}), false},
		{"restricting signer", mustDisk(t, "c", memory.New(), filesystem.WithURLSigner(restrictingSigner{recordingSigner{&last}, true})), true},
		{"signer reporting false", mustDisk(t, "d", memory.New(), filesystem.WithURLSigner(restrictingSigner{recordingSigner{&last}, false})), false},
		{"plain signer", mustDisk(t, "e", memory.New(), filesystem.WithURLSigner(recordingSigner{&last})), false},
	}
	for _, c := range cases {
		last = filesystem.PresignOptions{}
		_, err := c.disk.SignedURL(ctx, "a.txt", get)
		if c.ok && (err != nil || last.AllowedIP != get.AllowedIP) {
			t.Errorf("%s: %v, signed %+v", c.name, err, last)
		}
		if !c.ok && (filesystem.CodeOf(err) != filesystem.CodeUnsupported || last.AllowedIP != "") {
			t.Errorf("%s: want CodeUnsupported before signing, got %v (signed %+v)", c.name, err, last)
		}
	}
	d := cases[0].disk
	for _, bad := range []string{"203.0.113.7", "203.0.113.7/24", "not-an-ip/8", "10.0.0.0/33"} {
		o := get
		o.AllowedIP = bad
		if _, err := d.SignedURL(ctx, "a.txt", o); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("AllowedIP %q: %v", bad, err)
		}
	}
	for _, good := range []string{"203.0.113.7/32", "2001:db8::/32", "0.0.0.0/0"} {
		if _, err := filesystem.ParseAllowedIP(good); err != nil {
			t.Errorf("ParseAllowedIP(%q) = %v", good, err)
		}
	}
}

// TestPresignIPCapability proves PresignIP needs Presign, is listed by
// String, and that a routed disk declares it only when every mount
// enforces it.
func TestPresignIPCapability(t *testing.T) {
	if _, err := filesystem.NewDisk("x", claims{memory.New(), filesystem.Capabilities{PresignIP: true}}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("PresignIP without Presign accepted: %v", err)
	}
	var last filesystem.PresignOptions
	restricted := mustDisk(t, "r", ipPresigner{Driver: memory.New(), restricts: true, last: &last})
	if !strings.Contains(restricted.Capabilities().String(), "presign-ip") {
		t.Errorf("caps %v", restricted.Capabilities())
	}
	signer := mustDisk(t, "s", memory.New(), filesystem.WithURLSigner(restrictingSigner{recordingSigner{&last}, true}))
	plain := mustDisk(t, "p", ipPresigner{Driver: memory.New(), last: &last})
	both, err := filesystem.Route("both", []filesystem.Mount{{Prefix: "a", Disk: restricted}, {Prefix: "b", Disk: signer}})
	if err != nil {
		t.Fatal(err)
	}
	if !both.Capabilities().PresignIP {
		t.Error("route over enforcing mounts lacks PresignIP")
	}
	mixed, err := filesystem.Route("mixed", []filesystem.Mount{{Prefix: "a", Disk: restricted}, {Prefix: "b", Disk: plain}})
	if err != nil {
		t.Fatal(err)
	}
	if mixed.Capabilities().PresignIP {
		t.Error("route with a non-enforcing mount declares PresignIP")
	}
}
