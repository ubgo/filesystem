package dimensions_test

import (
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/dimensions"
	"github.com/ubgo/filesystem/records"
)

// TestOtherTypesKeepTheirKeys pins B2 for the plugin alone: a file it
// does not measure keeps an application's own width and height (a video
// keeps contrib/video's), and a replacement from an image to such a file
// drops what the plugin wrote, except width and height for a video.
func TestOtherTypesKeepTheirKeys(t *testing.T) {
	e := newEnv(t)
	app := map[string]string{dimensions.MetaWidth: "1200", dimensions.MetaHeight: "630", "app": "kept"}
	f, err := e.record(t, "card.txt", []byte("hello"), records.NewFile{ContentType: "text/plain", Metadata: app})
	if err != nil {
		t.Fatal(err)
	}
	if f.Metadata[dimensions.MetaWidth] != "1200" || f.Metadata[dimensions.MetaHeight] != "630" || f.Metadata["app"] != "kept" {
		t.Fatalf("a text file's own keys were cleared: %v", f.Metadata)
	}
	img, err := e.record(t, "a.png", pngOf(t, 80, 30), records.NewFile{ContentType: "image/png", Metadata: map[string]string{"app": "kept"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.disk.WriteBytes(t.Context(), "a.txt", []byte("now text"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	next, err := e.store.ReplaceObject(t.Context(), "t", img.ID, records.NewContent{Key: "a.txt", ContentType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	if dims(next) != [3]int{0, 0, 0} || next.Metadata["app"] != "kept" {
		t.Fatalf("image replaced by text: %v", next.Metadata)
	}
	// Replaced by a video, width and height are contrib/video's to set
	// (not installed here, so they keep the image's values); the
	// image-only keys go.
	thumbed := newEnv(t, dimensions.WithThumbHash(imaging.Options{}))
	img, err = thumbed.record(t, "b.png", pngOf(t, 80, 30), records.NewFile{ContentType: "image/png"})
	if err != nil || img.Metadata[dimensions.MetaThumbHash] == "" {
		t.Fatalf("image = %v, %v", img.Metadata, err)
	}
	if _, err := thumbed.disk.WriteBytes(t.Context(), "b.mp4", []byte("not read"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	vid, err := thumbed.store.ReplaceObject(t.Context(), "t", img.ID, records.NewContent{Key: "b.mp4", ContentType: "video/mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if vid.Metadata[dimensions.MetaWidth] != "80" || vid.Metadata[dimensions.MetaHeight] != "30" || vid.Metadata[dimensions.MetaThumbHash] != "" {
		t.Fatalf("image replaced by video: %v", vid.Metadata)
	}
}
