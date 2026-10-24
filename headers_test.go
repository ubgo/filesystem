package filesystem_test

import (
	"context"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// noCopy is the memory driver without server-side copy, so Copy streams.
type noCopy struct{ *memory.Driver }

func (n noCopy) Capabilities() filesystem.Capabilities {
	c := n.Driver.Capabilities()
	c.Copy = false
	return c
}

func TestStreamCopyKeepsHeaders(t *testing.T) {
	ctx := t.Context()
	d, err := filesystem.NewDisk("h", noCopy{memory.New()})
	if err != nil {
		t.Fatal(err)
	}
	h := filesystem.HTTPHeaders{CacheControl: "no-store", ContentDisposition: "inline"}
	if _, err := d.WriteBytes(ctx, "a", []byte("x"), filesystem.WriteOptions{Headers: h}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Copy(ctx, "a", "b"); err != nil {
		t.Fatal(err)
	}
	if e, _ := d.Stat(ctx, "b"); e.Headers != h {
		t.Fatalf("copied headers = %+v", e.Headers)
	}
	if !(filesystem.HTTPHeaders{}).IsZero() || h.IsZero() {
		t.Fatal("IsZero")
	}
}

// recordingSigner keeps the options of the last URL it signed.
type recordingSigner struct{ last *filesystem.PresignOptions }

func (r recordingSigner) SignURL(_ context.Context, _, key string, o filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	*r.last = o
	return filesystem.SignedRequest{Method: o.Method, URL: "https://signed.test/" + key}, nil
}

func TestSignedDownloadsOfActiveContentAreAttachments(t *testing.T) {
	var last filesystem.PresignOptions
	d, _ := filesystem.NewDisk("s", memory.New(), filesystem.WithURLSigner(recordingSigner{&last}))
	for _, c := range []struct{ ct, given, want string }{
		{"image/svg+xml", "", "attachment"},
		{"text/html; charset=utf-8", "", "attachment"},
		{"image/svg+xml", `attachment; filename="a.svg"`, `attachment; filename="a.svg"`},
		{"image/png", "", ""},
	} {
		_, err := d.SignedURL(t.Context(), "k", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute, ContentType: c.ct, ContentDisposition: c.given})
		if err != nil || last.ContentDisposition != c.want {
			t.Errorf("%s with %q: signed %q, %v", c.ct, c.given, last.ContentDisposition, err)
		}
	}
}
