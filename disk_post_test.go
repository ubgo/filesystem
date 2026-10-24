package filesystem_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// postSigner is a memory driver that presigns POST forms, recording the
// policy it was given; headers says whether it stores headers.
type postSigner struct {
	*memory.Driver
	last    *filesystem.PostPolicy
	headers bool
}

func (p postSigner) Capabilities() filesystem.Capabilities {
	c := p.Driver.Capabilities()
	c.PresignPost, c.Headers = true, p.headers
	return c
}

func (p postSigner) PresignPost(_ context.Context, pol filesystem.PostPolicy) (filesystem.PresignedPost, error) {
	*p.last = pol
	return filesystem.PresignedPost{URL: "https://bucket.test/", Fields: map[string]string{filesystem.PostFieldKey: pol.Key}, ExpiresAt: fixedNow.Add(pol.Expires)}, nil
}

func TestPresignPost(t *testing.T) {
	ctx := t.Context()
	var last filesystem.PostPolicy
	var ops []filesystem.Op
	rec := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		ops = append(ops, *op)
		return call(ctx)
	}
	d := mustDisk(t, "media", postSigner{Driver: memory.New(), last: &last, headers: true}, filesystem.WithInterceptors(rec))
	ok := filesystem.PostPolicy{Key: "/a//b.png", ContentType: "image/png", MaxSize: 10, Expires: time.Minute, Headers: filesystem.HTTPHeaders{CacheControl: "no-store"}}
	post, err := d.PresignPost(ctx, ok)
	if err != nil || post.URL == "" || last.Key != "a/b.png" || last.Headers.CacheControl != "no-store" {
		t.Fatalf("post %+v, policy %+v, %v", post, last, err)
	}
	if op := ops[len(ops)-1]; op.Kind != filesystem.OpPresignPost || !op.Mutating || op.Key != "a/b.png" {
		t.Errorf("op %+v", op)
	}
	if _, err := d.PresignPost(ctx, filesystem.PostPolicy{KeyPrefix: "/in/", MaxSize: 10, Expires: time.Minute}); err != nil || last.KeyPrefix != "in" || last.Key != "" {
		t.Errorf("prefix policy %+v, %v", last, err)
	}
	bad := map[string]filesystem.PostPolicy{
		"no key":          {MaxSize: 10, Expires: time.Minute},
		"key and prefix":  {Key: "a", KeyPrefix: "b", MaxSize: 10, Expires: time.Minute},
		"no max size":     {Key: "a", Expires: time.Minute},
		"negative min":    {Key: "a", MinSize: -1, MaxSize: 10, Expires: time.Minute},
		"min over max":    {Key: "a", MinSize: 11, MaxSize: 10, Expires: time.Minute},
		"no expiry":       {Key: "a", MaxSize: 10},
		"too long":        {Key: "a", MaxSize: 10, Expires: filesystem.MaxPresignExpiry + time.Second},
		"split header":    {Key: "a", MaxSize: 10, Expires: time.Minute, Headers: filesystem.HTTPHeaders{CacheControl: "a\r\nb: c"}},
		"root prefix":     {KeyPrefix: "/", MaxSize: 10, Expires: time.Minute},
		"traversal":       {Key: "a/../../b", MaxSize: 10, Expires: time.Minute},
		"prefix escaping": {KeyPrefix: "../x", MaxSize: 10, Expires: time.Minute},
	}
	for name, p := range bad {
		_, err := d.PresignPost(ctx, p)
		if code := filesystem.CodeOf(err); code != filesystem.CodeInvalidArgument && code != filesystem.CodeInvalidKey {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Stored headers need a driver that stores them.
	plain := mustDisk(t, "plain", postSigner{Driver: memory.New(), last: &last})
	_, err = plain.PresignPost(ctx, ok)
	wantCode(t, err, filesystem.CodeUnsupported)
	// No capability, a read-only disk, a mirror: refused.
	_, err = mustDisk(t, "mem", memory.New()).PresignPost(ctx, filesystem.PostPolicy{Key: "a", MaxSize: 1, Expires: time.Minute})
	wantCode(t, err, filesystem.CodeUnsupported)
	ro := mustDisk(t, "ro", postSigner{Driver: memory.New(), last: &last}, filesystem.WithInterceptors(filesystem.ReadOnly()))
	_, err = ro.PresignPost(ctx, filesystem.PostPolicy{Key: "a", MaxSize: 1, Expires: time.Minute})
	wantCode(t, err, filesystem.CodeReadOnly)
	m, err := filesystem.Mirror("m", d, mustDisk(t, "copy", memory.New()))
	if err != nil {
		t.Fatal(err)
	}
	if m.Capabilities().PresignPost || !strings.Contains(d.Capabilities().String(), "presign-post") {
		t.Errorf("mirror caps %v, primary %v", m.Capabilities(), d.Capabilities())
	}
	// A driver error is returned annotated.
	failing := mustDisk(t, "f", failingPost{postSigner{Driver: memory.New(), last: &last}})
	_, err = failing.PresignPost(ctx, filesystem.PostPolicy{Key: "a", MaxSize: 1, Expires: time.Minute})
	wantCode(t, err, filesystem.CodeUnavailable)
}

// failingPost fails every PresignPost.
type failingPost struct{ postSigner }

func (failingPost) PresignPost(context.Context, filesystem.PostPolicy) (filesystem.PresignedPost, error) {
	return filesystem.PresignedPost{}, filesystem.ErrUnavailable
}

// A scoped disk's forms stay inside the scope.
func TestScopedPresignPost(t *testing.T) {
	var last filesystem.PostPolicy
	d := mustDisk(t, "media", postSigner{Driver: memory.New(), last: &last})
	tenant, err := d.Scope("acme", "tenants/acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.PresignPost(t.Context(), filesystem.PostPolicy{Key: "a.png", MaxSize: 1, Expires: time.Minute}); err != nil || last.Key != "tenants/acme/a.png" {
		t.Errorf("scoped key %+v, %v", last, err)
	}
	if _, err := tenant.PresignPost(t.Context(), filesystem.PostPolicy{KeyPrefix: "in", MaxSize: 1, Expires: time.Minute}); err != nil || last.KeyPrefix != "tenants/acme/in" {
		t.Errorf("scoped prefix %+v, %v", last, err)
	}
}
