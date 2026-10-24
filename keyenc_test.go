package filesystem_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/filesystemtest"
	"github.com/ubgo/filesystem/memory"
)

// windowsLike escapes what SFTP-on-Windows and Azure refuse.
var windowsLike = filesystem.KeyEscaper{Chars: `:*?<>|"\`, SegmentEnd: ". "}

func TestKeyEscaper(t *testing.T) {
	for key, want := range map[string]string{
		"plain/key.txt": "plain/key.txt",
		"a:b*c?.txt":    "a%3Ab%2Ac%3F.txt",
		"100%/x":        "100%25/x",
		"dir./name. ":   "dir%2E/name.%20",
		`q"<>|`:         "q%22%3C%3E%7C",
		"café/ü:é.txt":  "café/ü%3Aé.txt",
	} {
		got := windowsLike.EncodeKey(key)
		if got != want {
			t.Errorf("EncodeKey(%q) = %q, want %q", key, got, want)
		}
		back, err := windowsLike.DecodeKey(got)
		if err != nil || back != key {
			t.Errorf("DecodeKey(%q) = %q, %v", got, back, err)
		}
	}
	// A non-ASCII character to escape takes one escape per byte.
	multi := filesystem.KeyEscaper{Chars: "é"}
	if got := multi.EncodeKey("é"); got != "%C3%A9" {
		t.Errorf("multi-byte escape %q", got)
	}
	for _, bad := range []string{"a%", "a%4", "a%zz", "%41", "a%3a", "x%2Fy", "a:b", "dir./x", "%25%"} {
		if _, err := windowsLike.DecodeKey(bad); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
			t.Errorf("DecodeKey(%q) = %v", bad, err)
		}
	}
	// The zero value escapes only the escape mark.
	if got := (filesystem.KeyEscaper{}).EncodeKey("a:b%c"); got != "a:b%25c" {
		t.Errorf("zero escaper %q", got)
	}
}

// pickyStore is a memory driver that refuses names a Windows-like backend
// cannot store, and so encodes keys with a KeyEncoder at its boundary.
type pickyStore struct {
	*memory.Driver
	enc filesystem.KeyEncoder
}

func (p pickyStore) name(key string) string { return p.enc.EncodeKey(key) }

func (p pickyStore) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	e, err := p.Driver.Stat(ctx, p.name(key))
	e.Key = key
	return e, err
}

func (p pickyStore) Read(ctx context.Context, key string, r filesystem.ByteRange) (io.ReadCloser, error) {
	return p.Driver.Read(ctx, p.name(key), r)
}

func (p pickyStore) Write(ctx context.Context, key string, body io.Reader, o filesystem.WriteOptions) (filesystem.Entry, error) {
	name := p.name(key)
	if strings.ContainsAny(name, `:*?<>|"`) {
		return filesystem.Entry{}, filesystem.ErrInvalidKey
	}
	e, err := p.Driver.Write(ctx, name, body, o)
	e.Key = key
	return e, err
}

func (p pickyStore) Delete(ctx context.Context, key string) error {
	return p.Driver.Delete(ctx, p.name(key))
}

func (p pickyStore) List(ctx context.Context, dir string, o filesystem.ListOptions) (filesystem.Page, error) {
	page, err := p.Driver.List(ctx, p.name(dir), o)
	for i, e := range page.Entries {
		if page.Entries[i].Key, err = p.enc.DecodeKey(e.Key); err != nil {
			return filesystem.Page{}, err
		}
	}
	return page, err
}

func (pickyStore) Capabilities() filesystem.Capabilities { return filesystem.Capabilities{} }

// The conformance suite's key-characters case passes over a backend that
// could not store those characters, through the encoder.
func TestKeyEncoderUnderTheConformanceSuite(t *testing.T) {
	for _, c := range filesystemtest.Cases() {
		if c.Name != "KeyCharactersRoundTrip" && c.Name != "WriteThenRead" && c.Name != "ListRecursive" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			d := mustDisk(t, "picky", pickyStore{memory.New(), windowsLike})
			if err := c.Check(t.Context(), d); err != nil {
				t.Fatal(err)
			}
		})
	}
	// Without the encoder the same backend refuses the key.
	raw := mustDisk(t, "raw", pickyStore{memory.New(), filesystem.KeyEscaper{}})
	_, err := raw.WriteBytes(t.Context(), "a:b", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeInvalidKey)
}
