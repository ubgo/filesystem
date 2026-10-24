package video_test

import (
	"bytes"
	"testing"

	"github.com/ubgo/filesystem/contrib/video"
	"github.com/ubgo/filesystem/records"
)

// TestPlugin_QuarantinedVideo: a quarantined upload is recorded without
// running ffprobe or making a poster from its unscanned bytes (it
// succeeds even with no ffprobe at all), and is described, with its
// poster, when its scan passes.
func TestPlugin_QuarantinedVideo(t *testing.T) {
	requirePrograms(t)
	ctx := t.Context()
	body := clip(t)
	missing := newEnv(t, video.NewPlugin(video.Tool{FFprobe: "/nonexistent/ffprobe"}))
	if _, err := missing.store.Upload(ctx, scope, "media", bytes.NewReader(body), "clip.mp4", videoPolicy, records.Quarantine()); err != nil {
		t.Fatalf("a quarantined upload ran the describer: %v", err)
	}

	e := newEnv(t, video.NewPlugin(video.Tool{}, video.WithPoster(0)))
	f, err := e.store.Upload(ctx, scope, "media", bytes.NewReader(body), "clip.mp4", videoPolicy, records.Quarantine())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := video.Of(f); ok {
		t.Fatalf("a quarantined file was described: %v", f.Metadata)
	}
	if _, ok := video.PosterKey(f); ok {
		t.Fatal("a quarantined file got a poster")
	}
	got, err := e.store.PassScan(ctx, scope, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := video.Of(got); !ok || m.Width == 0 {
		t.Fatalf("after release: %+v, %v", m, ok)
	}
	key, ok := video.PosterKey(got)
	if !ok || !e.exists(t, key) {
		t.Fatalf("no poster after release: %q %v", key, ok)
	}
}
