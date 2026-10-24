package filesystem_test

import (
	"context"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// visibilityPresigner is a memory driver that presigns PUTs and POST forms
// and records what it was asked, pinning a visibility when pins is set.
// acl false drops SetVisibility, as a bucket without per-object ACLs.
type visibilityPresigner struct {
	*memory.Driver
	pins, acl bool
	last      *filesystem.PresignOptions
	lastPost  *filesystem.PostPolicy
}

func (v visibilityPresigner) Capabilities() filesystem.Capabilities {
	c := v.Driver.Capabilities()
	c.SetVisibility = v.acl
	c.Presign, c.PresignPost, c.PresignVisibility = true, true, v.pins
	return c
}

func (v visibilityPresigner) Presign(_ context.Context, key string, o filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	*v.last = o
	return filesystem.SignedRequest{Method: o.Method, URL: "https://backend.test/" + key}, nil
}

func (v visibilityPresigner) PresignPost(_ context.Context, p filesystem.PostPolicy) (filesystem.PresignedPost, error) {
	*v.lastPost = p
	return filesystem.PresignedPost{URL: "https://backend.test/", Fields: map[string]string{filesystem.PostFieldKey: p.Key}}, nil
}

func TestPresignVisibility(t *testing.T) {
	ctx := t.Context()
	put := func(v filesystem.Visibility) filesystem.PresignOptions {
		return filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, Visibility: v}
	}
	post := func(v filesystem.Visibility) filesystem.PostPolicy {
		return filesystem.PostPolicy{Key: "a.png", MaxSize: 1, Expires: time.Minute, Visibility: v}
	}
	for _, c := range []struct {
		name      string
		pins, acl bool
		opts      []filesystem.Option
		ask       filesystem.Visibility
		want      filesystem.Visibility // what the driver is handed
		code      filesystem.Code
	}{
		{name: "default unchanged: nothing asked, nothing pinned", pins: true, acl: true, ask: "", want: ""},
		{name: "pinning driver gets public", pins: true, acl: true, ask: filesystem.VisibilityPublic, want: filesystem.VisibilityPublic},
		{name: "pinning driver gets private", pins: true, acl: true, ask: filesystem.VisibilityPrivate, want: filesystem.VisibilityPrivate},
		{name: "ACL driver that cannot pin refuses", acl: true, ask: filesystem.VisibilityPublic, code: filesystem.CodeUnsupported},
		{name: "ACL driver that cannot pin refuses even private", acl: true, ask: filesystem.VisibilityPrivate, code: filesystem.CodeUnsupported},
		{name: "no ACLs: the default needs no pin", ask: filesystem.VisibilityPrivate, want: ""},
		{name: "no ACLs: another value is refused", ask: filesystem.VisibilityPublic, code: filesystem.CodeUnsupported},
		{name: "no ACLs, public default: public needs no pin", opts: []filesystem.Option{filesystem.WithDefaultVisibility(filesystem.VisibilityPublic)}, ask: filesystem.VisibilityPublic, want: ""},
		{name: "unknown value", pins: true, acl: true, ask: "world", code: filesystem.CodeInvalidArgument},
	} {
		t.Run(c.name, func(t *testing.T) {
			var last filesystem.PresignOptions
			var lastPost filesystem.PostPolicy
			last.Visibility, lastPost.Visibility = "unset", "unset"
			d := mustDisk(t, "v", visibilityPresigner{Driver: memory.New(), pins: c.pins, acl: c.acl, last: &last, lastPost: &lastPost}, c.opts...)
			_, err := d.SignedURL(ctx, "a.png", put(c.ask))
			if filesystem.CodeOf(err) != c.code {
				t.Fatalf("SignedURL: %v, want code %q", err, c.code)
			}
			_, perr := d.PresignPost(ctx, post(c.ask))
			if filesystem.CodeOf(perr) != c.code {
				t.Fatalf("PresignPost: %v, want code %q", perr, c.code)
			}
			if c.code != "" {
				if last.Visibility != "unset" || lastPost.Visibility != "unset" {
					t.Fatal("the driver was called for a refused request")
				}
				return
			}
			if last.Visibility != c.want || lastPost.Visibility != c.want {
				t.Fatalf("driver got PUT %q, POST %q; want %q", last.Visibility, lastPost.Visibility, c.want)
			}
		})
	}
}

func TestPresignVisibilityRefusedOnGetAndThroughSigner(t *testing.T) {
	ctx := t.Context()
	var last filesystem.PresignOptions
	d := mustDisk(t, "v", visibilityPresigner{Driver: memory.New(), pins: true, acl: true, last: &last, lastPost: new(filesystem.PostPolicy)})
	_, err := d.SignedURL(ctx, "a.png", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute, Visibility: filesystem.VisibilityPublic})
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("GET with a visibility: %v", err)
	}
	// A URLSigner signs uploads a Write then stores at the default: it
	// never pins a visibility, so an ACL disk refuses to sign one.
	s := mustDisk(t, "s", memory.New(), filesystem.WithURLSigner(recordingSigner{&last}))
	_, err = s.SignedURL(ctx, "a.png", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, Visibility: filesystem.VisibilityPrivate})
	if filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("signer PUT with a visibility: %v", err)
	}
	if _, err := s.SignedURL(ctx, "a.png", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute}); err != nil {
		t.Errorf("signer PUT without a visibility: %v", err)
	}
}

func TestPresignVisibilityDeclaration(t *testing.T) {
	// PresignVisibility without SetVisibility is a lie NewDisk refuses.
	_, err := filesystem.NewDisk("v", visibilityPresigner{Driver: memory.New(), pins: true, last: new(filesystem.PresignOptions), lastPost: new(filesystem.PostPolicy)})
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("NewDisk: %v", err)
	}
	// Wrappers that refuse uploads straight to the backend drop it.
	inner := mustDisk(t, "p", visibilityPresigner{Driver: memory.New(), pins: true, acl: true, last: new(filesystem.PresignOptions), lastPost: new(filesystem.PostPolicy)})
	m, err := filesystem.Mirror("m", inner, mustDisk(t, "b", memory.New()))
	if err != nil {
		t.Fatal(err)
	}
	if m.Capabilities().PresignVisibility {
		t.Error("a mirror declares PresignVisibility")
	}
	if !inner.Capabilities().PresignVisibility {
		t.Error("the pinning disk does not declare PresignVisibility")
	}
}

func TestRoutePresignVisibility(t *testing.T) {
	ctx := t.Context()
	var last filesystem.PresignOptions
	pinning := func(name string) *filesystem.Disk {
		return mustDisk(t, name, visibilityPresigner{Driver: memory.New(), pins: true, acl: true, last: &last, lastPost: new(filesystem.PostPolicy)})
	}
	r, err := filesystem.Route("r", []filesystem.Mount{{Prefix: "a", Disk: pinning("a")}, {Disk: pinning("b")}})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Capabilities().PresignVisibility {
		t.Fatal("a route over pinning mounts does not declare PresignVisibility")
	}
	if _, err := r.SignedURL(ctx, "a/x.png", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, Visibility: filesystem.VisibilityPublic}); err != nil || last.Visibility != filesystem.VisibilityPublic {
		t.Fatalf("routed PUT: %v, driver got %q", err, last.Visibility)
	}
	// One mount that cannot pin takes the capability from the route.
	r, err = filesystem.Route("r", []filesystem.Mount{{Prefix: "a", Disk: pinning("a")}, {Disk: mustDisk(t, "b", visibilityPresigner{Driver: memory.New(), acl: true, last: &last, lastPost: new(filesystem.PostPolicy)})}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Capabilities().PresignVisibility {
		t.Fatal("a route with a mount that cannot pin declares PresignVisibility")
	}
}
