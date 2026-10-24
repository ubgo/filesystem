package audio_test

import (
	"bytes"
	"testing"

	"github.com/ubgo/filesystem/contrib/audio"
	"github.com/ubgo/filesystem/records"
)

// TestPlugin_QuarantinedAudio: a quarantined upload is recorded without
// running ffprobe on its unscanned bytes (it succeeds even with no
// ffprobe at all), and is described when its scan passes.
func TestPlugin_QuarantinedAudio(t *testing.T) {
	ctx := t.Context()
	missing := newStore(t, audio.NewPlugin(audio.Tool{FFprobe: "/nonexistent/ffprobe"}))
	if _, err := missing.Upload(ctx, scope, "media", bytes.NewReader(ramp(t)), "note.wav", policy, records.Quarantine()); err != nil {
		t.Fatalf("a quarantined upload ran the describer: %v", err)
	}

	store := newStore(t, audio.NewPlugin(audio.Tool{}, audio.WithWaveform(8)))
	f, err := store.Upload(ctx, scope, "media", bytes.NewReader(ramp(t)), "note.wav", policy, records.Quarantine())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := audio.Of(f); ok || f.Metadata[audio.MetaWaveform] != "" {
		t.Fatalf("a quarantined file was described: %v", f.Metadata)
	}
	got, err := store.PassScan(ctx, scope, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := audio.Of(got); !ok || m.Duration != rampSeconds*1e9 {
		t.Fatalf("after release: %+v, %v", m, ok)
	}
	if read, _ := store.File(ctx, scope, f.ID); read.Metadata[audio.MetaWaveform] == "" {
		t.Fatalf("the release's metadata was not stored: %v", read.Metadata)
	}
}
