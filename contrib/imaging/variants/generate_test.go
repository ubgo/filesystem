package variants_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// eventLog is a records plugin that records every event it hears, and
// the status a created file has once its transaction committed.
type eventLog struct {
	mu     sync.Mutex
	events []records.Event
	// committed is the status read back right after each create
	// committed, before any other after-commit work.
	committed []records.Status
}

func (l *eventLog) Name() string { return "event_log" }
func (l *eventLog) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	if e.Kind == records.EventFileCreated {
		s.AfterCommit(ctx, func(ctx context.Context) {
			f, err := s.File(ctx, e.Scope, e.FileID)
			if err == nil {
				l.mu.Lock()
				l.committed = append(l.committed, f.Status)
				l.mu.Unlock()
			}
		})
	}
	return nil
}

// of returns the events of kind, in order.
func (l *eventLog) of(kind records.EventKind) []records.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []records.Event
	for _, e := range l.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

type genEnv struct {
	*env
	log *eventLog
}

// newGenEnv builds a store with the plugin and an event log, on a disk
// with the given interceptors.
func newGenEnv(t *testing.T, p *variants.Plugin, opts ...filesystem.Option) *genEnv {
	t.Helper()
	db, _ := recordstest.Open(t)
	disk, _ := filesystem.NewDisk("media", memory.New(), opts...)
	mgr, _ := filesystem.NewManager(disk)
	l := &eventLog{}
	// The log first, so its after-commit read runs before the plugin's
	// eager generation.
	s, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(l, p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &genEnv{env: &env{db: db, disk: disk, store: s, plugin: p}, log: l}
}

func (g *genEnv) file(t *testing.T, id string) records.File {
	t.Helper()
	f, err := g.store.File(t.Context(), "t", id)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// statuses lists the statuses file.status_changed reported, in order.
func (g *genEnv) statuses() []string {
	var out []string
	for _, e := range g.log.of(records.EventFileStatusChanged) {
		out = append(out, string(e.File.Status)+" "+e.File.StatusDetail)
	}
	return out
}

// TestEagerReportsStatusAndEvents pins EV-03 for eager generation: the
// file is processing from the change's own transaction, then ready, and
// variants.ready carries the file and the variants made.
func TestEagerReportsStatusAndEvents(t *testing.T) {
	p, _ := variants.New(transforms, imaging.Options{}, variants.WithEager(nil, "thumb", "square"))
	g := newGenEnv(t, p)
	f := g.image(t, "a.png", pngBytes(t, 60, 60), filesystem.VisibilityPrivate)
	if got := g.statuses(); len(got) != 2 || got[0] != "processing variants 0/2" || got[1] != "ready " {
		t.Fatalf("statuses = %q", got)
	}
	ready := g.log.of(variants.EventReady)
	if len(ready) != 1 || ready[0].File == nil || ready[0].File.Status != records.StatusReady || ready[0].Detail[variants.DetailVariants] != "thumb,square" || ready[0].FileID != f.ID {
		t.Fatalf("ready events = %+v", ready)
	}
	if st := g.file(t, f.ID).Status; st != records.StatusReady {
		t.Errorf("status = %s", st)
	}
	// Committed already processing: no reader saw it ready too early.
	if len(g.log.committed) != 1 || g.log.committed[0] != records.StatusProcessing {
		t.Errorf("status at commit = %v", g.log.committed)
	}
	// A non-image is never marked.
	_, _ = g.disk.WriteBytes(t.Context(), "n.txt", []byte("text"), filesystem.WriteOptions{})
	if _, err := g.store.CreateFile(t.Context(), records.NewFile{Scope: "t", Disk: "media", Key: "n.txt"}); err != nil {
		t.Fatal(err)
	}
	if got := g.statuses(); len(got) != 2 {
		t.Errorf("a text file changed status: %q", got)
	}
}

// TestEagerFailureMarksTheFileFailed pins the failure path: the status is
// failed with a code (never a message, since clients read it), the
// event names the variant, and WithEager's handler still hears it.
func TestEagerFailureMarksTheFileFailed(t *testing.T) {
	var heard []string
	p, _ := variants.New(transforms, imaging.Options{}, variants.WithEager(func(_ context.Context, _ records.File, name string, _ error) {
		heard = append(heard, name)
	}, "thumb", "square"))
	failVariantWrites := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpWrite && strings.HasPrefix(op.Key, variants.KeyDir+"/") && strings.Contains(op.Key, "/square-") {
			return filesystem.Errorf(filesystem.CodeUnavailable, "injected")
		}
		return call(ctx)
	}
	g := newGenEnv(t, p, filesystem.WithInterceptors(failVariantWrites))
	f := g.image(t, "a.png", pngBytes(t, 60, 60), filesystem.VisibilityPrivate)
	got := g.file(t, f.ID)
	if got.Status != records.StatusFailed || got.StatusDetail != "square: unavailable" {
		t.Fatalf("status = %s %q", got.Status, got.StatusDetail)
	}
	failed := g.log.of(variants.EventFailed)
	if len(failed) != 1 || failed[0].Detail[variants.DetailFailed] != "square" || failed[0].Detail[variants.DetailCode] != "unavailable" || failed[0].Detail[variants.DetailVariants] != "thumb" {
		t.Fatalf("failed events = %+v", failed)
	}
	if len(heard) != 1 || heard[0] != "square" {
		t.Errorf("handler heard %v", heard)
	}
	if len(g.log.of(variants.EventReady)) != 0 {
		t.Error("a failed run also emitted ready")
	}
}

func TestGenerate(t *testing.T) {
	ctx := t.Context()
	p, _ := variants.New(transforms, imaging.Options{})
	g := newGenEnv(t, p)
	f := g.image(t, "a.png", pngBytes(t, 60, 60), filesystem.VisibilityPrivate)
	if err := p.Generate(ctx, g.store, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if g.rows(t) != 2 || len(g.log.of(variants.EventReady)) != 1 || g.file(t, f.ID).Status != records.StatusReady {
		t.Fatalf("rows %d, ready events %d", g.rows(t), len(g.log.of(variants.EventReady)))
	}
	// Running again finds the variants and reports ready again.
	if err := p.Generate(ctx, g.store, "t", f.ID, "thumb"); err != nil || g.rows(t) != 2 || len(g.log.of(variants.EventReady)) != 2 {
		t.Errorf("rerun = %v, rows %d", err, g.rows(t))
	}
	if err := p.Generate(ctx, g.store, "t", f.ID, "huge"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("undeclared name = %v", err)
	}
	if err := p.Generate(ctx, g.store, "t", "fil_missing"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("missing file = %v", err)
	}
	// A non-image has nothing to make.
	_, _ = g.disk.WriteBytes(ctx, "n.txt", []byte("text"), filesystem.WriteOptions{})
	n, _ := g.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "n.txt"})
	if err := p.Generate(ctx, g.store, "t", n.ID); err != nil || g.file(t, n.ID).Status != records.StatusReady {
		t.Errorf("text = %v", err)
	}
	// A trashed file left processing by this plugin is set ready again,
	// and the call says not_found. Another plugin's processing status is
	// left alone.
	if _, err := g.store.SetStatus(ctx, "t", f.ID, records.StatusProcessing, "variants 0/2"); err != nil {
		t.Fatal(err)
	}
	if err := g.store.Trash(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.Generate(ctx, g.store, "t", f.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound || g.file(t, f.ID).Status != records.StatusReady {
		t.Errorf("trashed = %v, status %s", err, g.file(t, f.ID).Status)
	}
	if _, err := g.store.SetStatus(ctx, "t", n.ID, records.StatusProcessing, "transcoding"); err != nil {
		t.Fatal(err)
	}
	_ = p.Generate(ctx, g.store, "t", n.ID)
	if got := g.file(t, n.ID); got.Status != records.StatusProcessing || got.StatusDetail != "transcoding" {
		t.Errorf("another plugin's status was changed: %s %q", got.Status, got.StatusDetail)
	}
}

func TestGenerateMissing(t *testing.T) {
	ctx := t.Context()
	p, _ := variants.New(transforms, imaging.Options{})
	g := newGenEnv(t, p)
	var ids []string
	for _, k := range []string{"a.png", "b.png", "c.png"} {
		ids = append(ids, g.image(t, k, pngBytes(t, 40, 40), filesystem.VisibilityPrivate).ID)
	}
	_, _ = g.disk.WriteBytes(ctx, "n.txt", []byte("text"), filesystem.WriteOptions{})
	_, _ = g.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "n.txt"})
	// b already has both.
	if err := p.Generate(ctx, g.store, "t", ids[1]); err != nil {
		t.Fatal(err)
	}
	// In runs of two files: resumable through the cursor.
	var total variants.MissingResult
	cursor := ""
	for range 5 {
		res, err := p.GenerateMissing(ctx, g.store, variants.MissingOptions{Scope: "t", Cursor: cursor, MaxFiles: 2, PageSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		total.Generated += res.Generated
		total.Complete += res.Complete
		if cursor = res.NextCursor; cursor == "" {
			break
		}
	}
	if total.Generated != 2 || total.Complete != 1 || cursor != "" || g.rows(t) != 6 {
		t.Fatalf("total = %+v, rows %d", total, g.rows(t))
	}
	// Again: all complete. A newly named subset is checked by name.
	res, err := p.GenerateMissing(ctx, g.store, variants.MissingOptions{Scope: "t", Names: []string{"thumb"}})
	if err != nil || res.Complete != 3 || res.Generated != 0 {
		t.Errorf("second run = %+v, %v", res, err)
	}
	if _, err := p.GenerateMissing(ctx, g.store, variants.MissingOptions{Scope: "t", Names: []string{"huge"}}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("undeclared name = %v", err)
	}
	if _, err := p.GenerateMissing(ctx, g.store, variants.MissingOptions{Scope: "t", Cursor: "bogus"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("bad cursor = %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.GenerateMissing(cancelled, g.store, variants.MissingOptions{Scope: "t"}); err == nil {
		t.Error("a cancelled context did not stop the run")
	}
}

// TestGenerateMissingRecordsFailures pins that one file's failure is
// listed and the run carries on.
func TestGenerateMissingRecordsFailures(t *testing.T) {
	ctx := t.Context()
	p, _ := variants.New(transforms, imaging.Options{})
	g := newGenEnv(t, p)
	good := g.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPrivate)
	_, _ = g.disk.WriteBytes(ctx, "broken.png", []byte("\x89PNG\r\n\x1a\nbroken"), filesystem.WriteOptions{})
	bad, _ := g.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "broken.png", ContentType: "image/png"})
	res, err := p.GenerateMissing(ctx, g.store, variants.MissingOptions{Scope: "t"})
	if err != nil || res.Generated != 1 || len(res.Failed) != 1 || res.Failed[0].FileID != bad.ID || filesystem.CodeOf(res.Failed[0].Err) != filesystem.CodeRejected {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if g.file(t, bad.ID).Status != records.StatusFailed || g.file(t, good.ID).Status != records.StatusReady {
		t.Error("statuses")
	}
}

// TestGenerateDatabaseFailures covers the status writes failing (a
// trigger refuses them) and the lookup of existing variants failing.
func TestGenerateDatabaseFailures(t *testing.T) {
	recordstest.OnlySQLite(t) // injects failures with SQLite triggers
	ctx := t.Context()
	p, _ := variants.New(transforms, imaging.Options{}, variants.WithEager(nil, "thumb"))
	g := newGenEnv(t, p)
	f := g.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPrivate)
	refuse := func(status string) {
		t.Helper()
		if _, err := g.db.Exec("CREATE TRIGGER no_" + status + " BEFORE UPDATE ON " + records.TableFiles.Name + " WHEN NEW.status = '" + status + "' BEGIN SELECT RAISE(FAIL, 'injected'); END"); err != nil {
			t.Fatal(err)
		}
	}
	refuse("processing")
	// Eager: the change itself fails, so nothing is half done.
	_, _ = g.disk.WriteBytes(ctx, "b.png", pngBytes(t, 40, 40), filesystem.WriteOptions{})
	if _, err := g.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "b.png"}); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Errorf("eager status failure = %v", err)
	}
	if err := p.Generate(ctx, g.store, "t", f.ID); err == nil {
		t.Error("Generate ignored a failed processing status")
	}
	_, _ = g.db.Exec("DROP TRIGGER no_processing")
	refuse("ready")
	if err := p.Generate(ctx, g.store, "t", f.ID, "square"); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Errorf("final status failure = %v", err)
	}
	_, _ = g.db.Exec("DROP TRIGGER no_ready")
	_, _ = g.db.Exec("DROP TABLE " + variants.TableNameVariants)
	if _, err := p.GenerateMissing(ctx, g.store, variants.MissingOptions{Scope: "t"}); err == nil {
		t.Error("GenerateMissing ignored a failed lookup")
	}
}

// TestGenerateMissingStopsWhenTheContextEnds pins that a run cancelled
// while generating stops before the next file.
func TestGenerateMissingStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stop := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpRead {
			cancel()
		}
		return call(ctx)
	}
	p, _ := variants.New(transforms, imaging.Options{})
	g := newGenEnv(t, p, filesystem.WithInterceptors(stop))
	g.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPrivate)
	g.image(t, "b.png", pngBytes(t, 40, 40), filesystem.VisibilityPrivate)
	res, err := p.GenerateMissing(ctx, g.store, variants.MissingOptions{Scope: "t"})
	if err == nil || res.NextCursor == "" {
		t.Errorf("cancelled run = %+v, %v", res, err)
	}
}
