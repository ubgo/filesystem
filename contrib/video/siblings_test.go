package video_test

import (
	"bytes"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video"
	"github.com/ubgo/filesystem/records"
)

// TestPlugin_LeavesOtherTypesKeys pins B2: the plugin's keys are cleared
// only on videos. width and height are shared with the dimensions plugin
// and duration_ms and audio_codec with contrib/audio, so an image, an
// audio file or an application's own file keeps them.
func TestPlugin_LeavesOtherTypesKeys(t *testing.T) {
	e := newEnv(t, video.NewPlugin(video.Tool{}, video.WithPoster(0)))
	own := map[string]string{video.MetaWidth: "1200", video.MetaHeight: "630", video.MetaDuration: "9000", video.MetaAudioCodec: "aac", "app": "kept"}
	for i, ct := range []string{"image/png", "audio/mpeg", "text/plain"} {
		key := []string{"a.png", "a.mp3", "a.txt"}[i]
		if _, err := e.disk.WriteBytes(t.Context(), key, []byte("not read"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		f, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: key, ContentType: ct, Metadata: own})
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range own {
			if f.Metadata[k] != v {
				t.Fatalf("%s: %s = %q, want %q (%v)", ct, k, f.Metadata[k], v, f.Metadata)
			}
		}
	}
	// A video replaced by an image: width and height are the dimensions
	// plugin's to set (not installed here, so they keep the video's
	// values); the video-only keys and the poster go.
	v, err := e.upload(t, clip(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.disk.WriteBytes(t.Context(), "still.png", []byte("not read"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	img, err := e.store.ReplaceObject(t.Context(), scope, v.ID, records.NewContent{Key: "still.png", ContentType: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	if img.Metadata[video.MetaWidth] != "320" || img.Metadata[video.MetaHeight] != "240" {
		t.Fatalf("the keys shared with the dimensions plugin went: %v", img.Metadata)
	}
	for _, k := range []string{video.MetaDuration, video.MetaVideoCodec, video.MetaAudioCodec, video.MetaPosterKey} {
		if _, ok := img.Metadata[k]; ok {
			t.Fatalf("%s outlived the video: %v", k, img.Metadata)
		}
	}
}

// TestReplaceLeavesNoKeys: a video holding only the plugin's keys,
// replaced by another type, ends with no metadata at all.
func TestReplaceLeavesNoKeys(t *testing.T) {
	body := clip(t)
	e := newEnv(t, video.NewPlugin(video.Tool{}))
	f, err := e.upload(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := video.Of(f); !ok {
		t.Fatalf("video = %v", f.Metadata)
	}
	next, err := e.store.Replace(t.Context(), scope, f.ID, bytes.NewReader([]byte("now text\n")), "a.txt", videoPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Metadata) != 0 {
		t.Fatalf("video replaced by text kept %v", next.Metadata)
	}
}
