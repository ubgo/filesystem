package upload_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/sniff"
	"github.com/ubgo/filesystem/upload"
)

// A format Go has no signature for: http.DetectContentType calls it
// octet-stream, and only fakeDetector recognises it.
const (
	fakeModel     = "\x00\x01FAKE3D\x00\x00model"
	fakeModelType = "model/x-fake3d"
)

// fakeDetector labels fakeModel and nothing else.
func fakeDetector(head []byte, _ string) string {
	if bytes.HasPrefix(head, []byte("\x00\x01FAKE3D")) {
		return fakeModelType
	}
	return ""
}

// models is a strict allowlist naming only the custom format.
func models(detect sniff.Detector) upload.Policy {
	return upload.Policy{MaxSize: 1 << 20, Allowed: []string{fakeModelType}, Detector: detect}
}

// TestPolicyDetectorLabelsUnknownFormats pins Policy.Detector on Receive:
// it lets a strict allowlist accept a format the built-in detection does
// not know, without allowing octet-stream, and the default (no detector)
// still refuses it.
func TestPolicyDetectorLabelsUnknownFormats(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, memory.New())
	res, err := upload.Receive(ctx, d, reader(fakeModel), "part.f3d", models(fakeDetector))
	if err != nil || res.ContentType != fakeModelType {
		t.Fatalf("Receive with detector = %+v, %v", res, err)
	}
	_, err = upload.Receive(ctx, d, reader(fakeModel), "part.f3d", models(nil))
	wantCode(t, err, filesystem.CodeRejected)
}

// TestPolicyDetectorCannotWeakenThePolicy pins the limits on a detector:
// it cannot relabel a type the built-in detection knows (HTML stays HTML),
// an empty or malformed answer stays octet-stream and is refused, and an
// active type it returns still meets the active-content floor.
func TestPolicyDetectorCannotWeakenThePolicy(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, memory.New())
	always := func([]byte, string) string { return fakeModelType }
	_, err := upload.Receive(ctx, d, reader(htmlData), "page.f3d", models(always))
	wantCode(t, err, filesystem.CodeRejected)

	for _, answer := range []string{"", "  ", "not a type;;"} {
		_, err := upload.Receive(ctx, d, reader(fakeModel), "part.f3d", models(func([]byte, string) string { return answer }))
		wantCode(t, err, filesystem.CodeRejected)
	}

	html := func([]byte, string) string { return "text/html" }
	p := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"*/*"}, Detector: html}
	_, err = upload.Receive(ctx, d, reader(fakeModel), "part.f3d", p)
	wantCode(t, err, filesystem.CodeRejected)
}

// TestPolicyDetectorOnEveryPath pins that Confirm (presigned and Uppy
// uploads) and the chunked handler detect with the policy's Detector too.
func TestPolicyDetectorOnEveryPath(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, memory.New())
	if _, err := d.WriteBytes(ctx, "u/part.f3d", []byte(fakeModel), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err := upload.Confirm(ctx, d, "u/part.f3d", "part.f3d", models(fakeDetector))
	if err != nil || res.ContentType != fakeModelType {
		t.Fatalf("Confirm with detector = %+v, %v", res, err)
	}
	if _, err := d.WriteBytes(ctx, "u/other.f3d", []byte(fakeModel), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = upload.Confirm(ctx, d, "u/other.f3d", "other.f3d", models(nil))
	wantCode(t, err, filesystem.CodeRejected)

	for _, c := range []struct {
		detect sniff.Detector
		want   int
	}{{fakeDetector, http.StatusCreated}, {nil, http.StatusUnsupportedMediaType}} {
		h := chunked(t, newDisk(t, memory.New()), models(c.detect), upload.ChunkedOptions{Staging: "chunks", Owner: byUser})
		b, ct := chunkBody(t, upload.DropzoneFields(), "id", 0, 1, int64(len(fakeModel)), "part.f3d", fakeModel)
		if rec := sendChunk(t, h, "u1", b, ct); rec.Code != c.want {
			t.Errorf("chunked upload with detector %v = %d %s, want %d", c.detect != nil, rec.Code, rec.Body, c.want)
		}
	}
}
