package filesystem_test

import (
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// TestRouteSigningClaimsMatchTheMount pins that a routed disk claims
// Presign, PresignIP and PresignHeaders exactly when the mount's SignedURL
// delivers them under the per-method signer rule (WithURLSigner): every
// claim is checked against real calls on the mount, so neither an
// over-claim nor an under-claim passes.
func TestRouteSigningClaimsMatchTheMount(t *testing.T) {
	var last filesystem.PresignOptions
	restricting := func(r bool) filesystem.URLSigner {
		return restrictingSigner{recordingSigner: recordingSigner{last: &last}, restricts: r}
	}
	pinning := func(p bool) filesystem.URLSigner {
		return pinningSigner{recordingSigner: recordingSigner{last: &last}, pins: p}
	}
	ipDriver := func() filesystem.Driver { return ipPresigner{Driver: memory.New(), restricts: true, last: &last} }
	headerDriver := func() filesystem.Driver { return headerPresigner{Driver: memory.New(), pins: true, last: &last} }
	type build func(t *testing.T) *filesystem.Disk
	plainDisk := func(d filesystem.Driver, opts ...filesystem.Option) build {
		return func(t *testing.T) *filesystem.Disk { return mustDisk(t, "m", d, opts...) }
	}
	// Composite disks whose driver declares Presign but signs downloads
	// only (mirror, cache), directly and through the wrappers that forward
	// to them (scope, failover).
	mirrored := func(opts ...filesystem.Option) build {
		return func(t *testing.T) *filesystem.Disk {
			d, err := filesystem.Mirror("m", mustDisk(t, "primary", fullDriver{memory.New()}, opts...), mustDisk(t, "copy", memory.New()))
			if err != nil {
				t.Fatal(err)
			}
			return d
		}
	}
	cached := func(t *testing.T) *filesystem.Disk {
		d, err := filesystem.CachedRead("m", mustDisk(t, "remote", fullDriver{memory.New()}), mustDisk(t, "cache", memory.New()), filesystem.CacheOptions{TTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	scopedMirror := func(t *testing.T) *filesystem.Disk {
		d, err := mirrored()(t).Scope("m", "tenant")
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	failoverMirror := func(t *testing.T) *filesystem.Disk {
		d, err := filesystem.Failover("m", mirrored()(t), []*filesystem.Disk{mustDisk(t, "standby", memory.New())}, filesystem.FailoverOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	cases := []struct {
		name  string
		mount build
	}{
		{"no signer, no presign", plainDisk(memory.New())},
		{"presigning driver", plainDisk(fullDriver{memory.New()})},
		{"signer on a disk that cannot presign", plainDisk(memory.New(), filesystem.WithURLSigner(stubSigner{}))},
		// The over-claims this test was written for: the signer takes only
		// downloads on a driver that cannot presign, so PUT is unsupported.
		{"download-only signer, no presign", plainDisk(memory.New(), filesystem.WithURLSigner(stubSigner{}, filesystem.MethodGet))},
		// The driver restricts IPs but the signer, which takes downloads,
		// does not.
		{"IP driver, non-restricting signer", plainDisk(ipDriver(), filesystem.WithURLSigner(restricting(false)))},
		{"IP driver, restricting signer", plainDisk(ipDriver(), filesystem.WithURLSigner(restricting(true)))},
		{"restricting signer for both", plainDisk(memory.New(), filesystem.WithURLSigner(restricting(true)))},
		// The driver pins headers but the signer, given uploads, does not.
		{"header driver, non-pinning upload signer", plainDisk(headerDriver(), filesystem.WithURLSigner(pinning(false), filesystem.MethodGet, filesystem.MethodPut))},
		{"header driver, download signer", plainDisk(headerDriver(), filesystem.WithURLSigner(pinning(false)))},
		{"pinning signer, no presign", plainDisk(memory.New(), filesystem.WithURLSigner(pinning(true)))},
		{"mirror", mirrored()},
		{"mirror with a download signer", mirrored(filesystem.WithURLSigner(stubSigner{}))},
		{"mirror with a signer for both", mirrored(filesystem.WithURLSigner(stubSigner{}, filesystem.MethodGet, filesystem.MethodPut))},
		{"cache", cached},
		{"scoped mirror", scopedMirror},
		{"failover over a mirror", failoverMirror},
	}
	get := filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute}
	put := filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute}
	withIP := func(o filesystem.PresignOptions) filesystem.PresignOptions {
		o.AllowedIP = "203.0.113.0/24"
		return o
	}
	headed := put
	headed.Headers = filesystem.HTTPHeaders{CacheControl: "no-store"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mount := c.mount(t)
			r, err := filesystem.Route("r", []filesystem.Mount{{Disk: mount}})
			if err != nil {
				t.Fatal(err)
			}
			works := func(opts ...filesystem.PresignOptions) bool {
				for _, o := range opts {
					if _, err := mount.SignedURL(t.Context(), "k", o); err != nil {
						return false
					}
				}
				return true
			}
			caps := r.Capabilities()
			for _, claim := range []struct {
				name     string
				declared bool
				delivers bool
			}{
				{"Presign", caps.Presign, works(get, put)},
				{"PresignIP", caps.PresignIP, works(withIP(get), withIP(put))},
				{"PresignHeaders", caps.PresignHeaders, works(headed)},
			} {
				if claim.declared != claim.delivers {
					t.Errorf("route declares %s %t, the mount delivers it: %t", claim.name, claim.declared, claim.delivers)
				}
			}
		})
	}
}
