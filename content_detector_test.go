package filesystem_test

import (
	"bytes"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/sniff"
)

// TestWithContentDetector pins the Disk's detection hook: a write that
// declares no type gets the custom label for content the built-in
// detection does not know, a known type is never relabelled, the default
// is unchanged, a nil detector is refused, and turning detection off
// turns the detector off too.
func TestWithContentDetector(t *testing.T) {
	ctx := t.Context()
	const unknown, custom = "\x00\x01FAKE3D\x00", "model/x-fake3d"
	var detect sniff.Detector = func(head []byte, _ string) string {
		if bytes.HasPrefix(head, []byte("\x00\x01FAKE3D")) {
			return custom
		}
		return ""
	}
	typeOf := func(d *filesystem.Disk, key, body string) string {
		t.Helper()
		e, err := d.WriteBytes(ctx, key, []byte(body), filesystem.WriteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return e.ContentType
	}
	d := mustDisk(t, "d", memory.New(), filesystem.WithContentDetector(detect))
	if got := typeOf(d, "part.f3d", unknown); got != custom {
		t.Errorf("custom format stored as %q, want %q", got, custom)
	}
	if got := typeOf(d, "page.f3d", "<!DOCTYPE html><p>x"); got != "text/html; charset=utf-8" {
		t.Errorf("HTML stored as %q", got)
	}
	if got := typeOf(mustDisk(t, "d", memory.New()), "part.f3d", unknown); got != sniff.OctetStream {
		t.Errorf("default stored %q, want octet-stream", got)
	}
	off := mustDisk(t, "d", memory.New(), filesystem.WithContentDetector(detect), filesystem.WithoutContentTypeDetection())
	if got := typeOf(off, "part.f3d", unknown); got != "" {
		t.Errorf("detection off stored %q", got)
	}
	_, err := filesystem.NewDisk("d", memory.New(), filesystem.WithContentDetector(nil))
	wantCode(t, err, filesystem.CodeInvalidArgument)
}
