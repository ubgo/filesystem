package filesystem_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// fullwidthDots maps the fullwidth full stop to ".", as NFKC does: the
// kind of normaliser that could turn an innocent key into a traversal.
func fullwidthDots(s string) (string, error) {
	if strings.Contains(s, "\x00") {
		return "", errors.New("NUL in key")
	}
	return strings.ReplaceAll(s, "．", "."), nil
}

func TestKeyNormalizer(t *testing.T) {
	ctx := t.Context()
	if _, err := filesystem.NewDisk("d", memory.New(), filesystem.WithKeyNormalizer(nil)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("nil normalizer: %v", err)
	}
	d := mustDisk(t, "d", memory.New(), filesystem.WithKeyNormalizer(fullwidthDots))
	if _, err := d.WriteBytes(ctx, "docs/v1．0.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	// Both spellings address the one object, through every path.
	for _, key := range []string{"docs/v1.0.txt", "docs/v1．0.txt"} {
		if ok, err := d.Exists(ctx, key); !ok || err != nil {
			t.Errorf("Exists(%q) = %v, %v", key, ok, err)
		}
	}
	var listed []string
	for e, err := range d.List(ctx, "docs", filesystem.ListOptions{}) {
		if err != nil {
			t.Fatal(err)
		}
		listed = append(listed, e.Key)
	}
	if len(listed) != 1 || listed[0] != "docs/v1.0.txt" {
		t.Errorf("listed %v", listed)
	}
	if k, err := d.CleanKey("/docs//v1．0.txt"); err != nil || k != "docs/v1.0.txt" {
		t.Errorf("CleanKey = %q, %v", k, err)
	}
	// The traversal check runs on the normalised key.
	_, err := d.WriteBytes(ctx, "docs/．．/．．/etc/passwd", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeInvalidKey)
	_, err = d.Stat(ctx, "a\x00b")
	wantCode(t, err, filesystem.CodeInvalidKey)
	// Directories too, and a scoped disk keeps the normaliser.
	for _, dir := range []string{"．．", "a\x00"} {
		for _, err := range d.List(ctx, dir, filesystem.ListOptions{}) {
			wantCode(t, err, filesystem.CodeInvalidKey)
		}
	}
	tenant, err := d.Scope("t", "tenants/a")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = tenant.WriteBytes(ctx, "x．txt", []byte("y"), filesystem.WriteOptions{})
	if ok, _ := d.Exists(ctx, "tenants/a/x.txt"); !ok {
		t.Error("scoped disk lost the normaliser")
	}
	// Without a normaliser the method is the package function.
	plain := mustDisk(t, "p", memory.New())
	if k, _ := plain.CleanKey("a．b"); k != "a．b" {
		t.Errorf("plain CleanKey = %q", k)
	}
}

// A scope or DSN prefix is normalised like the keys under it, so the two
// spellings of a prefix name one directory.
func TestPrefixesAreNormalised(t *testing.T) {
	ctx := t.Context()
	inner := memory.New()
	d := mustDisk(t, "d", inner, filesystem.WithKeyNormalizer(fullwidthDots))
	scoped, err := d.Scope("t", "v1．0")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = scoped.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{})
	if ok, _ := d.Exists(ctx, "v1.0/a.txt"); !ok {
		t.Error("scope prefix not normalised")
	}
	m := filesystem.NewMux()
	_ = m.Register("mem", func(context.Context, *url.URL) (filesystem.Driver, error) { return inner, nil })
	viaDSN, err := m.Open(ctx, "dsn", "mem://?prefix=v2．0", filesystem.WithKeyNormalizer(fullwidthDots))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = viaDSN.WriteBytes(ctx, "b.txt", []byte("x"), filesystem.WriteOptions{})
	if ok, _ := d.Exists(ctx, "v2.0/b.txt"); !ok {
		t.Error("DSN prefix not normalised")
	}
	// A normaliser error in the code's options fails the open.
	_, err = m.Open(ctx, "bad", "mem://?prefix=x", filesystem.WithKeyNormalizer(nil))
	wantCode(t, err, filesystem.CodeInvalidArgument)
}
