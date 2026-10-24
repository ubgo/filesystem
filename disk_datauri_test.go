package filesystem_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// pngHeader is the PNG signature, enough for content sniffing.
var pngHeader = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

// statLiar reports a size of 1 for every object and counts reads, to show
// DataURI both refuses before reading and caps the read itself.
type statLiar struct {
	*memory.Driver
	lie   bool
	reads int
}

func (s *statLiar) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	e, err := s.Driver.Stat(ctx, key)
	if s.lie {
		e.Size = 1
	}
	return e, err
}

func (s *statLiar) Read(ctx context.Context, key string, r filesystem.ByteRange) (io.ReadCloser, error) {
	s.reads++
	return s.Driver.Read(ctx, key, r)
}

func newDataURIDisk(t *testing.T) (*filesystem.Disk, *statLiar) {
	t.Helper()
	drv := &statLiar{Driver: memory.New()}
	d, err := filesystem.NewDisk("assets", drv)
	if err != nil {
		t.Fatal(err)
	}
	return d, drv
}

func put(t *testing.T, d *filesystem.Disk, key, contentType string, body []byte) {
	t.Helper()
	if _, err := d.Write(context.Background(), key, bytes.NewReader(body), filesystem.WriteOptions{ContentType: contentType}); err != nil {
		t.Fatal(err)
	}
}

func TestDataURI(t *testing.T) {
	ctx := context.Background()
	d, _ := newDataURIDisk(t)
	put(t, d, "icons/dot.png", "", pngHeader)
	got, err := d.DataURI(ctx, "/icons/./dot.png", 64)
	if err != nil {
		t.Fatal(err)
	}
	if want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngHeader); got != want {
		t.Fatalf("DataURI = %q, want %q", got, want)
	}
	put(t, d, "hello.txt", `text/plain; charset="utf-8"; x="a,b"`, []byte("hi"))
	if got, err = d.DataURI(ctx, "hello.txt", 64); err != nil || got != "data:text/plain;charset=utf-8;base64,aGk=" {
		t.Fatalf("DataURI = %q, %v", got, err)
	}
	put(t, d, "inject.txt", `text/plain; charset="x,y;base64"`, []byte("hi"))
	if got, err = d.DataURI(ctx, "inject.txt", 64); err != nil || got != "data:text/plain;base64,aGk=" {
		t.Fatalf("charset injection: DataURI = %q, %v", got, err)
	}
}

func TestDataURIRefusesLargeBeforeReading(t *testing.T) {
	d, drv := newDataURIDisk(t)
	put(t, d, "big.png", "image/png", append(pngHeader, make([]byte, 100)...))
	_, err := d.DataURI(context.Background(), "big.png", 50)
	if filesystem.CodeOf(err) != filesystem.CodeTooLarge || drv.reads != 0 {
		t.Fatalf("err = %v, reads = %d; want too_large with no read", err, drv.reads)
	}
}

func TestDataURICapsReadWhenStatLies(t *testing.T) {
	d, drv := newDataURIDisk(t)
	put(t, d, "big.png", "image/png", append(pngHeader, make([]byte, 100)...))
	drv.lie = true
	if _, err := d.DataURI(context.Background(), "big.png", 50); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Fatalf("err = %v, want too_large", err)
	}
}

func TestDataURIRefusesActiveContent(t *testing.T) {
	ctx := context.Background()
	d, drv := newDataURIDisk(t)
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	for key, ct := range map[string]string{"a.html": "text/html; charset=utf-8", "a.svg": "image/svg+xml", "a.xml": "application/xml", "a.js": "text/javascript"} {
		put(t, d, key, ct, []byte("x"))
		if _, err := d.DataURI(ctx, key, 1024); filesystem.CodeOf(err) != filesystem.CodeRejected {
			t.Errorf("%s (%s): err = %v, want rejected", key, ct, err)
		}
	}
	if drv.reads != 0 {
		t.Fatalf("active types were read %d times before refusal", drv.reads)
	}
	// Stored as an image, but the bytes are SVG: the sniffed type wins.
	put(t, d, "disguised.png", "image/png", svg)
	if _, err := d.DataURI(ctx, "disguised.png", 1024); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("disguised SVG: err = %v, want rejected", err)
	}
	put(t, d, "page.bin", "application/octet-stream", []byte("<!DOCTYPE html><html><script>alert(1)</script>"))
	if _, err := d.DataURI(ctx, "page.bin", 1024); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("disguised HTML: err = %v, want rejected", err)
	}
}

func TestDataURIArguments(t *testing.T) {
	d, _ := newDataURIDisk(t)
	ctx := context.Background()
	if _, err := d.DataURI(ctx, "a.png", 0); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("zero limit: err = %v", err)
	}
	if _, err := d.DataURI(ctx, "../a.png", 10); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Fatalf("traversal: err = %v", err)
	}
	if _, err := d.DataURI(ctx, "missing.png", 10); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("missing: err = %v", err)
	}
	put(t, d, "odd", "not a type", []byte("plain words"))
	if got, err := d.DataURI(ctx, "odd", 64); err != nil || !strings.HasPrefix(got, "data:text/plain;charset=utf-8;base64,") {
		t.Fatalf("malformed stored type: %q, %v", got, err)
	}
}
