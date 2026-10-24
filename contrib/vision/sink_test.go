package vision_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/vision"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// indexSink stands in for a search index: it keeps labels per file, or
// fails with err.
type indexSink struct {
	mu  sync.Mutex
	got map[string][]string
	err error
}

func (s *indexSink) Add(_ context.Context, _ *records.Store, _, id string, names ...string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	s.got[id] = append(s.got[id], names...)
	return s.got[id], nil
}

func (s *indexSink) Tags(_ context.Context, _ *records.Store, _, id string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.got[id], s.err
}

// TestPlugin_CustomSink pins the sink seam: labels reach another sink
// (no tags plugin installed), and the sink's error is Label's error.
func TestPlugin_CustomSink(t *testing.T) {
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	sink := &indexSink{got: map[string][]string{}}
	vp := vision.NewPlugin(&fakeLabeler{labels: []vision.Label{{Name: "Dog", Score: 0.98}}}, sink, func(string) bool { return true }, vision.WithPrefix("auto:"))
	db, _ := recordstest.Open(t)
	store, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(vp))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	f, err := store.Upload(t.Context(), optedIn, "media", bytes.NewReader(pngHead), "dog.png", policy)
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.got[f.ID]; !slices.Equal(got, []string{"auto:Dog"}) {
		t.Fatalf("sink got %q", got)
	}
	sink.err = errors.New("index down")
	if _, err := vp.Label(t.Context(), store, optedIn, f.ID); !errors.Is(err, sink.err) {
		t.Fatalf("a failing sink: %v", err)
	}
}
