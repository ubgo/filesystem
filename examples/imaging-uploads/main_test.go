package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"beach.jpg: 370170 bytes uploaded, 168554 stored, 900x600",
		"thumbhash: ErYBBYCrdjuUWmaXb4VnhiCh2tkI",
		" 320w, ", " 640w, ", " 900w",
		"icon.jpg refused: rejected: filesystem: rejected: image is 120x120; it must be at least 200 pixels wide",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

// TestEXIFAuthorship pins the EXIF step: the source photo does carry a
// GPS IFD, the position and the camera (so the checks below are not
// vacuous), and the optimised output keeps Artist and Copyright alone.
func TestEXIFAuthorship(t *testing.T) {
	photo, err := photoWithEXIF(cameraEXIF(photoArtist, photoCopyright))
	if err != nil {
		t.Fatal(err)
	}
	src := exifTags(photo)
	if _, ok := src[tagGPSIFD]; !ok || src[tagMake] != cameraMake || !bytes.Contains(photo, []byte(gpsPosition)) {
		t.Fatalf("the fixture lacks its GPS IFD, position or camera: %v", src)
	}
	var out strings.Builder
	if err := exifAuthorship(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"exif by default: kept false",
		`exif with MetadataEXIFCopyright: 2 fields, Artist "Ada Lens", Copyright "(c) 2026 Ada Lens"`,
		"exif GPS kept: false, position anywhere in the file: false, camera make kept: false",
		"exif of a photo with neither field: kept false",
		"keeping an unknown kind: invalid_argument",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
