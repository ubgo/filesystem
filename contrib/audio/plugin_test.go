package audio_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/audio"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

const scope = "acme"

// newStore returns a records store on a memory disk "media" with p
// installed.
func newStore(t *testing.T, p *audio.Plugin) *records.Store {
	t.Helper()
	disk, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := filesystem.NewManager(disk)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := recordstest.Open(t)
	store, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestPlugin_RecordsTheMetadataAndTheWaveform(t *testing.T) {
	store := newStore(t, audio.NewPlugin(audio.Tool{}, audio.WithWaveform(8)))
	f, err := store.Upload(t.Context(), scope, "media", bytes.NewReader(ramp(t)), "note.wav", policy)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := audio.Of(f)
	if !ok || m.Duration != rampSeconds*1e9 || m.Codec != "pcm_s16le" || m.SampleRate != 44100 || m.Channels != 2 || m.BitRate == 0 {
		t.Fatalf("Of = %+v, %v", m, ok)
	}
	peaks, ok := audio.WaveformOf(f)
	if !ok || len(peaks) != 8 || peaks[0] != 0 || peaks[7] < 0.9 {
		t.Fatalf("WaveformOf = %v, %v", peaks, ok)
	}
	// Read back from the row, not only from Upload's result.
	got, err := store.File(t.Context(), scope, f.ID)
	if err != nil || got.Metadata[audio.MetaWaveform] != f.Metadata[audio.MetaWaveform] {
		t.Fatalf("stored = %v, %v", got.Metadata, err)
	}
	// A replacement is described afresh: a text file keeps none of it.
	next, err := store.Replace(t.Context(), scope, f.ID, bytes.NewReader([]byte("hello\n")), "note.txt", policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := audio.Of(next); ok || next.Metadata[audio.MetaWaveform] != "" {
		t.Fatalf("stale metadata after replace: %v", next.Metadata)
	}
}

func TestPlugin_UnreadableAudio(t *testing.T) {
	bogus := []byte("ID3\x04\x00\x00\x00\x00\x00\x00 not really")
	lenient := newStore(t, audio.NewPlugin(audio.Tool{}))
	f, err := lenient.Upload(t.Context(), scope, "media", bytes.NewReader(bogus), "a.mp3", policy)
	if err != nil || f.Metadata != nil {
		t.Fatalf("lenient: %v, %v", f.Metadata, err)
	}
	strict := newStore(t, audio.NewPlugin(audio.Tool{}, audio.WithStrict()))
	if _, err := strict.Upload(t.Context(), scope, "media", bytes.NewReader(bogus), "a.mp3", policy); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("strict: %v", err)
	}
	missing := newStore(t, audio.NewPlugin(audio.Tool{FFprobe: "/nonexistent/ffprobe"}))
	if _, err := missing.Upload(t.Context(), scope, "media", bytes.NewReader(ramp(t)), "a.wav", policy); !errors.Is(err, audio.ErrNoProgram) {
		t.Fatalf("a missing program must fail loudly: %v", err)
	}
}

func TestPlugin_WaveformBoundsAndHooks(t *testing.T) {
	// Clamped to the stored maximum.
	store := newStore(t, audio.NewPlugin(audio.Tool{}, audio.WithWaveform(1<<20)))
	f, err := store.Upload(t.Context(), scope, "media", bytes.NewReader(ramp(t)), "note.wav", policy)
	if peaks, _ := audio.WaveformOf(f); err != nil || len(peaks) != audio.MaxStoredWaveformPoints {
		t.Fatalf("points = %d, %v", len(peaks), err)
	}
	p := audio.NewPlugin(audio.Tool{})
	if p.Name() != audio.PluginName || p.AfterCreate(context.Background(), nil, records.File{}) != nil || p.BeforePurge(context.Background(), nil, records.File{}) != nil {
		t.Fatal("hooks")
	}
	if _, ok := audio.WaveformOf(records.File{Metadata: map[string]string{audio.MetaWaveform: "!!"}}); ok {
		t.Fatal("a foreign waveform value was decoded")
	}
	if _, ok := audio.Of(records.File{}); ok {
		t.Fatal("Of without metadata")
	}
	if err := p.BeforeCreate(t.Context(), store, &records.File{Disk: "absent", ContentType: "audio/ogg"}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("an unknown disk: %v", err)
	}
	if err := p.BeforeCreate(t.Context(), store, &records.File{Disk: "media", Key: "absent", ContentType: "audio/ogg"}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a missing object: %v", err)
	}
}
