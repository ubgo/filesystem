package audio_test

import (
	"bytes"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/audio"
	"github.com/ubgo/filesystem/records"
)

// TestPlugin_LeavesOtherTypesKeys pins B2: the plugin's keys are cleared
// only on audio files. duration_ms and audio_codec are shared with
// contrib/video, so a video (or an application's own file) keeps them;
// a replacement from audio to a video keeps those two, the video's to
// set, and drops the rest.
func TestPlugin_LeavesOtherTypesKeys(t *testing.T) {
	store := newStore(t, audio.NewPlugin(audio.Tool{}))
	disk, err := store.Disks().Disk("media")
	if err != nil {
		t.Fatal(err)
	}
	put := func(key string, body []byte) {
		if _, err := disk.WriteBytes(t.Context(), key, body, filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	put("clip.mp4", []byte("not read: no plugin describes it here"))
	own := map[string]string{audio.MetaDuration: "9000", audio.MetaCodec: "aac", audio.MetaChannels: "6", "app": "kept"}
	f, err := store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: "clip.mp4", ContentType: "video/mp4", Metadata: own})
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range own {
		if f.Metadata[k] != v {
			t.Fatalf("a video's %s = %q, want %q (%v)", k, f.Metadata[k], v, f.Metadata)
		}
	}
	a, err := store.Upload(t.Context(), scope, "media", bytes.NewReader(ramp(t)), "note.wav", policy)
	if err != nil {
		t.Fatal(err)
	}
	put("next.mp4", []byte("a video now"))
	next, err := store.ReplaceObject(t.Context(), scope, a.ID, records.NewContent{Key: "next.mp4", ContentType: "video/mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if next.Metadata[audio.MetaDuration] != a.Metadata[audio.MetaDuration] || next.Metadata[audio.MetaCodec] != a.Metadata[audio.MetaCodec] {
		t.Fatalf("the keys shared with contrib/video went: %v", next.Metadata)
	}
	for _, k := range []string{audio.MetaBitRate, audio.MetaSampleRate, audio.MetaChannels} {
		if _, ok := next.Metadata[k]; ok {
			t.Fatalf("%s outlived the audio: %v", k, next.Metadata)
		}
	}
}
