package vision_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/vision"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/records/tags"
	"github.com/ubgo/filesystem/upload"
)

const (
	optedIn = "acme"
	other   = "globex"
)

// pngHead is enough of a PNG for the upload's sniffing.
var pngHead = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")

var policy = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*", "text/plain"}}

// fakeLabeler returns fixed labels, or fails, and records each image.
type fakeLabeler struct {
	mu     sync.Mutex
	labels []vision.Label
	err    error
	seen   [][]byte
}

func (f *fakeLabeler) Labels(_ context.Context, r io.Reader) ([]vision.Label, error) {
	var b bytes.Buffer
	if _, err := b.ReadFrom(r); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, b.Bytes())
	return f.labels, f.err
}

func (f *fakeLabeler) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

type env struct {
	store *records.Store
	disk  *filesystem.Disk
	tags  *tags.Plugin
}

func newEnv(t *testing.T, l vision.Labeler, opts ...vision.Option) (*env, *vision.Plugin) {
	t.Helper()
	disk, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := filesystem.NewManager(disk)
	if err != nil {
		t.Fatal(err)
	}
	tp := tags.New()
	vp := vision.NewPlugin(l, tp, func(scope string) bool { return scope == optedIn }, opts...)
	db, _ := recordstest.Open(t)
	store, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(tp, vp))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &env{store: store, disk: disk, tags: tp}, vp
}

func (e *env) tagsOf(t *testing.T, scope, id string) []string {
	t.Helper()
	got, err := e.tags.Tags(t.Context(), e.store, scope, id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPlugin_TagsImagesOfOptedInScopes(t *testing.T) {
	l := &fakeLabeler{labels: []vision.Label{
		{Name: "Dog", Score: 0.98},
		{Name: "Font", Score: 0.6},
		{Name: "Golden retriever", Score: 0.91},
		{Name: strings.Repeat("x", 80), Score: 0.95},
		{Name: "dog ", Score: 0.9},
		{Name: "Pet", Score: 0.85},
	}}
	e, _ := newEnv(t, l, vision.WithPrefix("auto:"), vision.WithMaxLabels(4))
	ctx := t.Context()
	f, err := e.store.Upload(ctx, optedIn, "media", bytes.NewReader(pngHead), "dog.png", policy)
	if err != nil {
		t.Fatal(err)
	}
	// Below the score, too long a tag and a repeat are skipped; the rest
	// remain, prefixed.
	if got := e.tagsOf(t, optedIn, f.ID); !slices.Equal(got, []string{"auto:dog", "auto:golden retriever", "auto:pet"}) {
		t.Fatalf("tags = %q", got)
	}
	if l.calls() != 1 || !bytes.Equal(l.seen[0], pngHead) {
		t.Fatalf("the labeler saw %d images", l.calls())
	}
	page, err := e.tags.Files(ctx, e.store, records.FileQuery{Scope: optedIn}, "auto:dog")
	if err != nil || len(page.Files) != 1 || page.Files[0].ID != f.ID {
		t.Fatalf("files tagged dog = %+v, %v", page, err)
	}

	// A scope not opted in, another type, and a rolled-back upload cost
	// nothing.
	g, err := e.store.Upload(ctx, other, "media", bytes.NewReader(pngHead), "dog.png", policy)
	if err != nil || len(e.tagsOf(t, other, g.ID)) != 0 {
		t.Fatalf("other scope: %v", err)
	}
	if _, err := e.store.Upload(ctx, optedIn, "media", strings.NewReader("notes"), "n.txt", policy); err != nil {
		t.Fatal(err)
	}
	errAbort := errors.New("abort")
	err = e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := e.store.Upload(ctx, optedIn, "media", bytes.NewReader(pngHead), "dog.png", policy); err != nil {
			return err
		}
		return errAbort
	})
	if !errors.Is(err, errAbort) || l.calls() != 1 {
		t.Fatalf("labeler calls = %d, %v", l.calls(), err)
	}
}

func TestPlugin_FailuresGoToTheHandler(t *testing.T) {
	errDown := filesystem.Errorf(filesystem.CodeUnavailable, "labeler down")
	var mu sync.Mutex
	var got []error
	handler := vision.WithErrorHandler(func(_ context.Context, _ records.File, err error) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, err)
	})
	e, _ := newEnv(t, &fakeLabeler{err: errDown}, handler)
	f, err := e.store.Upload(t.Context(), optedIn, "media", bytes.NewReader(pngHead), "dog.png", policy)
	if err != nil {
		t.Fatalf("a labelling failure failed the upload: %v", err)
	}
	if len(got) != 1 || !errors.Is(got[0], errDown) || len(e.tagsOf(t, optedIn, f.ID)) != 0 {
		t.Fatalf("handler got %v", got)
	}
	// An image over the bound reaches the handler without a call.
	l := &fakeLabeler{}
	got = nil
	e, _ = newEnv(t, l, handler, vision.WithMaxInput(8))
	if _, err := e.store.Upload(t.Context(), optedIn, "media", bytes.NewReader(pngHead), "dog.png", policy); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || filesystem.CodeOf(got[0]) != filesystem.CodeTooLarge || l.calls() != 0 {
		t.Fatalf("handler got %v, %d calls", got, l.calls())
	}
}

func TestLabel_OnDemand(t *testing.T) {
	l := &fakeLabeler{labels: []vision.Label{{Name: "Beach", Score: 0.9}}}
	e, p := newEnv(t, l, vision.WithMinScore(0.95), vision.WithContentTypes("IMAGE/PNG"))
	ctx := t.Context()
	// Nothing passes the score: the file keeps the tags it has.
	f, err := e.store.Upload(ctx, optedIn, "media", bytes.NewReader(pngHead), "b.png", policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.tags.Add(ctx, e.store, optedIn, f.ID, "holiday"); err != nil {
		t.Fatal(err)
	}
	if got, err := p.Label(ctx, e.store, optedIn, f.ID); err != nil || !slices.Equal(got, []string{"holiday"}) {
		t.Fatalf("label = %q, %v", got, err)
	}
	g, err := e.store.Upload(ctx, other, "media", bytes.NewReader(pngHead), "b.png", policy)
	if err != nil {
		t.Fatal(err)
	}
	calls := l.calls()
	if _, err := p.Label(ctx, e.store, other, g.ID); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a scope not opted in: %v", err)
	}
	txt, err := e.store.Upload(ctx, optedIn, "media", strings.NewReader("notes"), "n.txt", policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Label(ctx, e.store, optedIn, txt.ID); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a text file: %v", err)
	}
	if _, err := p.Label(ctx, e.store, optedIn, "missing"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a missing file: %v", err)
	}
	if l.calls() != calls {
		t.Fatal("a refused file reached the labeler")
	}
	if p.Name() != vision.PluginName {
		t.Fatal("name")
	}
	// Without a scopes function nothing is opted in.
	none := vision.NewPlugin(l, e.tags, nil)
	if _, err := none.Label(ctx, e.store, optedIn, f.ID); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("nil scopes: %v", err)
	}
}

func TestPlugin_MaxLabelsKeepsTheBest(t *testing.T) {
	l := &fakeLabeler{labels: []vision.Label{{Name: "c", Score: 0.8}, {Name: "a", Score: 0.99}, {Name: "b", Score: 0.9}}}
	e, _ := newEnv(t, l, vision.WithMaxLabels(2))
	f, err := e.store.Upload(t.Context(), optedIn, "media", bytes.NewReader(pngHead), "x.png", policy)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.tagsOf(t, optedIn, f.ID); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("tags = %q", got)
	}
}
