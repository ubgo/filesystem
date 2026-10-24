package records_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/scan"
	"github.com/ubgo/filesystem/upload"
)

// quarantined uploads a PNG recorded as quarantined.
func (e *env) quarantined(t *testing.T, name string, opts ...records.UploadOption) records.File {
	t.Helper()
	f, err := e.store.Upload(t.Context(), "t", "media", strings.NewReader(pngData), name, images, append(opts, records.Quarantine())...)
	if err != nil || !f.Quarantined() || f.Status != records.StatusQuarantined {
		t.Fatalf("quarantined upload = %+v, %v", f, err)
	}
	return f
}

func wantQuarantined(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, records.ErrQuarantined) {
		t.Fatalf("err = %v, want ErrQuarantined", err)
	}
}

// TestQuarantineRefusesEveryRead walks each way the Store hands a file
// out: a quarantined file gets no link, download link, streamed read,
// view or public visibility, whatever File value the caller holds, and no
// ServeHooks plugin hears of the refused read. PassScan then opens all of
// them.
func TestQuarantineRefusesEveryRead(t *testing.T) {
	ctx := t.Context()
	meter := &serveLog{name: "meter"}
	e := newEnv(t, nil, records.WithPlugins(meter))
	f := e.quarantined(t, "a.png")

	_, err := e.store.URL(ctx, f, time.Minute)
	wantQuarantined(t, err)
	_, err = e.store.DownloadURL(ctx, f, time.Minute)
	wantQuarantined(t, err)
	// A stale value saying ready, and a value a plugin builds for a
	// derived object (a variant link), are judged by the row.
	stale := f
	stale.Status = records.StatusReady
	_, err = e.store.URL(ctx, stale, time.Minute)
	wantQuarantined(t, err)
	variant := records.File{ID: f.ID, Scope: f.Scope, Disk: "media", Key: f.Key, Visibility: filesystem.VisibilityPublic, Size: 1}
	_, err = e.store.URL(ctx, variant, time.Minute, records.ServedAs(records.ServeVariant, "thumb"))
	wantQuarantined(t, err)
	err = e.store.Served(ctx, records.Served{Kind: records.ServeStream, Scope: "t", FileID: f.ID, Bytes: 3})
	wantQuarantined(t, err)
	if len(meter.got) != 0 {
		t.Fatalf("refused reads reached the meter: %+v", meter.got)
	}
	ok, err := e.store.Viewed(ctx, "t", f.ID)
	if ok {
		t.Fatal("a quarantined file was viewed")
	}
	wantQuarantined(t, err)
	wantQuarantined(t, e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic))
	if got, _ := e.store.File(ctx, "t", f.ID); got.Visibility != filesystem.VisibilityPrivate {
		t.Fatalf("visibility = %q", got.Visibility)
	}
	// Making it private (what it is) and reading its row stay open.
	if err := e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPrivate); err != nil {
		t.Fatal(err)
	}

	if _, err := e.store.PassScan(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.URL(ctx, stale, time.Minute); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.store.Viewed(ctx, "t", f.ID); !ok || err != nil {
		t.Fatalf("Viewed after the scan = %v, %v", ok, err)
	}
	if err := e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic); err != nil {
		t.Fatal(err)
	}
	if len(meter.got) != 1 {
		t.Fatalf("served after release = %+v", meter.got)
	}
	// A link to a file whose row is gone is not judged: it was never the
	// quarantine's to refuse.
	if err := e.store.Served(ctx, records.Served{Kind: records.ServeStream, Scope: "t", FileID: "fil_gone"}); err != nil {
		t.Fatal(err)
	}
	// Nor is a read that names no file (bytes not tied to a record).
	if err := e.store.Served(ctx, records.Served{Kind: records.ServeStream, Scope: "t"}); err != nil {
		t.Fatal(err)
	}
}

// TestQuarantineViewOnceSpendsNothing: a quarantined view-once file's
// view is not spent by a refused read, so it is still there to be seen
// once after the scan.
func TestQuarantineViewOnceSpendsNothing(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f := e.quarantined(t, "once.png", records.ViewOnce())
	_, err := e.store.Viewed(ctx, "t", f.ID)
	wantQuarantined(t, err)
	if _, err := e.store.PassScan(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.store.Viewed(ctx, "t", f.ID); !ok || err != nil {
		t.Fatalf("first view after the scan = %v, %v", ok, err)
	}
}

// publicHook makes every new file public, as an application's
// BeforeCreate hook might.
type publicHook struct{}

func (publicHook) Name() string { return "public_hook" }
func (publicHook) BeforeCreate(_ context.Context, _ *records.Store, f *records.File) error {
	f.Visibility = filesystem.VisibilityPublic
	return nil
}
func (publicHook) AfterCreate(context.Context, *records.Store, records.File) error { return nil }
func (publicHook) BeforePurge(context.Context, *records.Store, records.File) error { return nil }

// TestQuarantineNeverPublic: a quarantined file cannot be recorded public
// (by the caller, by the upload policy or by a hook), and a public file
// cannot be quarantined; a refused upload leaves no object behind.
func TestQuarantineNeverPublic(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	if _, err := e.disk.WriteBytes(ctx, "p.png", []byte(pngData), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "p.png", Visibility: filesystem.VisibilityPublic, Status: records.StatusQuarantined})
	if !errors.Is(err, records.ErrPublicQuarantine) {
		t.Fatalf("public quarantined CreateFile = %v", err)
	}
	public := images
	public.Visibility = filesystem.VisibilityPublic
	public.Key = func(upload.Info) (string, error) { return "up/public.png", nil }
	_, err = e.store.Upload(ctx, "t", "media", strings.NewReader(pngData), "x.png", public, records.Quarantine())
	if !errors.Is(err, records.ErrPublicQuarantine) {
		t.Fatalf("public quarantined Upload = %v", err)
	}
	if ok, _ := e.disk.Exists(ctx, "up/public.png"); ok {
		t.Fatal("a refused quarantined upload left its object")
	}

	hooked := newEnv(t, nil, records.WithPlugins(publicHook{}))
	_, err = hooked.store.Upload(ctx, "t", "media", strings.NewReader(pngData), "x.png", images, records.Quarantine())
	if !errors.Is(err, records.ErrPublicQuarantine) {
		t.Fatalf("a hook made a quarantined file public: %v", err)
	}

	f := e.put(t, "t", "pub.png")
	if err := e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic); err != nil {
		t.Fatal(err)
	}
	_, err = e.store.SetStatus(ctx, "t", f.ID, records.StatusQuarantined, "rescan")
	if !errors.Is(err, records.ErrPublicQuarantine) {
		t.Fatalf("quarantining a public file = %v", err)
	}
}

// TestQuarantineStatusGuard: SetStatus cannot release a quarantined file
// (a processing plugin finishing its work must not), but may quarantine a
// private file and change a quarantined file's detail.
func TestQuarantineStatusGuard(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f := e.quarantined(t, "a.png")
	for _, st := range []records.Status{records.StatusReady, records.StatusProcessing, records.StatusFailed} {
		_, err := e.store.SetStatus(ctx, "t", f.ID, st, "")
		wantQuarantined(t, err)
	}
	got, err := e.store.SetStatus(ctx, "t", f.ID, records.StatusQuarantined, "queued for scan")
	if err != nil || !got.Quarantined() || got.StatusDetail != "queued for scan" {
		t.Fatalf("detail change = %+v, %v", got, err)
	}
	g := e.put(t, "t", "b.png")
	if got, err := e.store.SetStatus(ctx, "t", g.ID, records.StatusQuarantined, "rescan"); err != nil || !got.Quarantined() {
		t.Fatalf("quarantining a private file = %+v, %v", got, err)
	}
	_, err = e.store.URL(ctx, g, time.Minute)
	wantQuarantined(t, err)
}

// TestQuarantineDedup: Dedup never records a new file on a quarantined
// file's unscanned object; a quarantined upload may reuse a ready one.
func TestQuarantineDedup(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	q := e.quarantined(t, "q.png")
	f, err := e.store.Upload(ctx, "t", "media", strings.NewReader(pngData), "f.png", images, records.Dedup())
	if err != nil {
		t.Fatal(err)
	}
	if f.Key == q.Key {
		t.Fatal("Dedup reused a quarantined object")
	}
	q2 := e.quarantined(t, "q2.png", records.Dedup())
	if q2.Key != f.Key {
		t.Fatalf("a quarantined upload did not reuse the ready object: %q vs %q", q2.Key, f.Key)
	}
}

// TestPassScan: the release is one status change and event; a second
// verdict, or one for a file never quarantined, changes nothing.
func TestPassScan(t *testing.T) {
	ctx := t.Context()
	r := &recorder{}
	e := newEnv(t, nil, records.WithPlugins(r))
	f := e.quarantined(t, "a.png")
	before := len(r.events)
	got, err := e.store.PassScan(ctx, "t", f.ID)
	if err != nil || got.Status != records.StatusReady || got.StatusDetail != "" {
		t.Fatalf("PassScan = %+v, %v", got, err)
	}
	if len(r.events) != before+2 || r.events[before].Kind != records.EventFileStatusChanged || r.events[before].File.Status != records.StatusReady ||
		r.events[before+1].Kind != records.EventFileReleased || r.events[before+1].File.Status != records.StatusReady {
		t.Fatalf("events = %+v", r.events[before:])
	}
	if got, err := e.store.PassScan(ctx, "t", f.ID); err != nil || got.Status != records.StatusReady || len(r.events) != before+2 {
		t.Fatalf("second PassScan = %+v, %v, %d events", got, err, len(r.events)-before)
	}
	_, err = e.store.PassScan(ctx, "t", "fil_nope")
	wantCode(t, err, filesystem.CodeNotFound)
	g := e.quarantined(t, "b.png")
	for _, kind := range []records.EventKind{records.EventFileStatusChanged, records.EventFileReleased} {
		r.refuse = kind
		if _, err := e.store.PassScan(ctx, "t", g.ID); err == nil {
			t.Fatalf("a refused %s was ignored", kind)
		}
		if read, _ := e.store.File(ctx, "t", g.ID); !read.Quarantined() {
			t.Fatal("a failed release changed the file")
		}
	}
}

// describer is a ReleaseHooks plugin that describes a file at release,
// tries to change what it must not, and fails when err is set.
type describer struct {
	seen []records.File
	err  error
	text string
}

func (d *describer) Name() string { return "describer" }
func (d *describer) BeforeRelease(_ context.Context, _ *records.Store, f *records.File) error {
	d.seen = append(d.seen, *f)
	if d.err != nil {
		return d.err
	}
	f.Metadata = map[string]string{"width": d.text, records.MetaAlt: d.text}
	f.Key, f.Status, f.Filename = "elsewhere.png", records.StatusFailed, "renamed.png"
	return nil
}

// TestReleaseHooks: a ReleaseHooks plugin sees the quarantined file being
// released and may set its metadata, nothing else; its error, or
// metadata over the limits, keeps the file quarantined.
func TestReleaseHooks(t *testing.T) {
	ctx := t.Context()
	d := &describer{text: "640"}
	e := newEnv(t, nil, records.WithPlugins(d))
	f := e.quarantined(t, "a.png")
	got, err := e.store.PassScan(ctx, "t", f.ID)
	if err != nil || got.Metadata["width"] != "640" || got.Key != f.Key || got.Filename != f.Filename || got.Status != records.StatusReady {
		t.Fatalf("PassScan = %+v, %v", got, err)
	}
	if read, _ := e.store.File(ctx, "t", f.ID); read.Metadata["width"] != "640" || read.Key != f.Key || read.Status != records.StatusReady {
		t.Fatalf("read back %+v", read)
	}
	if len(d.seen) != 1 || d.seen[0].Status != records.StatusReady {
		t.Fatalf("the hook saw %+v", d.seen)
	}
	if _, err := e.store.PassScan(ctx, "t", f.ID); err != nil || len(d.seen) != 1 {
		t.Fatalf("a second release ran the hook: %v, %d", err, len(d.seen))
	}
	g := e.quarantined(t, "b.png")
	d.err = errors.New("cannot describe")
	if _, err := e.store.PassScan(ctx, "t", g.ID); !errors.Is(err, d.err) {
		t.Fatalf("a failing hook = %v", err)
	}
	d.err, d.text = nil, "bad\x00text"
	_, err = e.store.PassScan(ctx, "t", g.ID)
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if read, _ := e.store.File(ctx, "t", g.ID); !read.Quarantined() || len(read.Metadata) != 0 {
		t.Fatalf("a refused release changed the file: %+v", read)
	}
}

// TestFailScan covers both failure actions and the guards.
func TestFailScan(t *testing.T) {
	ctx := t.Context()
	r := &recorder{}
	e := newEnv(t, nil, records.WithPlugins(r))
	f := e.quarantined(t, "a.png")
	got, err := e.store.FailScan(ctx, "t", f.ID, "infected: Eicar")
	if err != nil || !got.Quarantined() || got.StatusDetail != "infected: Eicar" {
		t.Fatalf("FailScan keep = %+v, %v", got, err)
	}
	before := len(r.events)
	if _, err := e.store.FailScan(ctx, "t", f.ID, "infected: Eicar"); err != nil || len(r.events) != before {
		t.Fatalf("a repeated verdict = %v, %d events", err, len(r.events)-before)
	}
	if !e.exists(t, f.Key) {
		t.Fatal("ScanFailureKeep deleted the object")
	}
	_, err = e.store.FailScan(ctx, "t", f.ID, "bad\x00detail")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = e.store.FailScan(ctx, "t", "fil_nope", "x")
	wantCode(t, err, filesystem.CodeNotFound)
	ready := e.put(t, "t", "ready.png")
	if _, err := e.store.FailScan(ctx, "t", ready.ID, "late"); !errors.Is(err, records.ErrNotQuarantined) {
		t.Fatalf("FailScan of a released file = %v", err)
	}
	if read, _ := e.store.File(ctx, "t", ready.ID); read.Status != records.StatusReady {
		t.Fatal("a late verdict changed a released file")
	}

	p := newEnv(t, nil, records.WithScanFailure(records.ScanFailurePurge))
	g := p.quarantined(t, "g.png")
	got, err = p.store.FailScan(ctx, "t", g.ID, "infected: Eicar")
	if err != nil || got.ID != g.ID {
		t.Fatalf("FailScan purge = %+v, %v", got, err)
	}
	_, err = p.store.File(ctx, "t", g.ID)
	wantCode(t, err, filesystem.CodeNotFound)
	if p.exists(t, g.Key) {
		t.Fatal("ScanFailurePurge kept the object")
	}

	mgr, _ := filesystem.NewManager(e.disk)
	if _, err := records.New(e.db, testDialect(), mgr, records.WithScanFailure("shred")); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || !strings.Contains(err.Error(), "shred") {
		t.Fatalf("an unknown action = %v", err)
	}
	if len(records.ScanFailureValues) != 2 {
		t.Fatal(records.ScanFailureValues)
	}
}

// TestScan runs a scanner over quarantined files: clean releases,
// infected fails with the signature cleaned of control characters, and a
// scan that does not finish leaves the file quarantined.
func TestScan(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	var read string
	clean := scan.ScannerFunc(func(_ context.Context, r io.Reader) (scan.Verdict, error) {
		b, err := io.ReadAll(r)
		read = string(b)
		return scan.Verdict{Clean: true}, err
	})
	f := e.quarantined(t, "a.png")
	got, err := e.store.Scan(ctx, "t", f.ID, clean)
	if err != nil || got.Status != records.StatusReady || read != pngData {
		t.Fatalf("clean Scan = %+v, %v (read %q)", got, err, read)
	}
	read = ""
	if got, err := e.store.Scan(ctx, "t", f.ID, clean); err != nil || got.Status != records.StatusReady || read != "" {
		t.Fatalf("Scan of a released file = %+v, %v, read %q", got, err, read)
	}

	sig := "Win.Test\x07.EICAR\xff" + strings.Repeat("x", 2*records.MaxStatusDetailChars)
	infected := scan.ScannerFunc(func(context.Context, io.Reader) (scan.Verdict, error) {
		return scan.Verdict{Signature: sig}, nil
	})
	g := e.quarantined(t, "b.png")
	got, err = e.store.Scan(ctx, "t", g.ID, infected)
	if err != nil || !got.Quarantined() || !strings.HasPrefix(got.StatusDetail, "infected: Win.Test.EICARxx") || len([]rune(got.StatusDetail)) != records.MaxStatusDetailChars {
		t.Fatalf("infected Scan = %q, %v", got.StatusDetail, err)
	}

	down := errors.New("clamd down")
	h := e.quarantined(t, "c.png")
	_, err = e.store.Scan(ctx, "t", h.ID, scan.ScannerFunc(func(context.Context, io.Reader) (scan.Verdict, error) { return scan.Verdict{}, down }))
	if !errors.Is(err, down) {
		t.Fatalf("failed scan = %v", err)
	}
	if read, _ := e.store.File(ctx, "t", h.ID); !read.Quarantined() || read.StatusDetail != "" {
		t.Fatalf("a failed scan changed the file: %+v", read)
	}
	_, err = e.store.Scan(ctx, "t", "fil_nope", clean)
	wantCode(t, err, filesystem.CodeNotFound)
	if err := e.disk.Delete(ctx, h.Key); err != nil {
		t.Fatal(err)
	}
	_, err = e.store.Scan(ctx, "t", h.ID, clean)
	wantCode(t, err, filesystem.CodeNotFound)
	// A file recorded on a disk the Store no longer has cannot be read.
	exec(t, e.db, "UPDATE "+records.TableNameFiles+" SET "+records.ColFileDisk+" = 'gone' WHERE "+records.ColFileID+" = '"+h.ID+"'")
	_, err = e.store.Scan(ctx, "t", h.ID, clean)
	wantCode(t, err, filesystem.CodeNotFound)
}

// racyDriver runs before when its object's visibility is changed: the
// window between SetVisibility's first read and its write.
type racyDriver struct {
	*memory.Driver
	before func()
}

func (d racyDriver) SetVisibility(ctx context.Context, key string, v filesystem.Visibility) error {
	d.before()
	return d.Driver.SetVisibility(ctx, key, v)
}

// TestQuarantineVisibilityRace: a file quarantined while SetVisibility is
// making it public is refused under the row lock, and its object goes
// back to private; a file purged in that window is not found.
func TestQuarantineVisibilityRace(t *testing.T) {
	ctx := t.Context()
	var before func()
	e := newEnv(t, racyDriver{Driver: memory.New(), before: func() { before() }})
	f := e.put(t, "t", "a.png")
	before = func() {
		exec(t, e.db, "UPDATE "+records.TableNameFiles+" SET "+records.ColFileStatus+" = '"+string(records.StatusQuarantined)+"' WHERE "+records.ColFileID+" = '"+f.ID+"'")
	}
	wantQuarantined(t, e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic))
	if st, _ := e.disk.Stat(ctx, f.Key); st.Visibility != filesystem.VisibilityPrivate {
		t.Fatalf("object left %q", st.Visibility)
	}
	g := e.put(t, "t", "b.png")
	before = func() {
		exec(t, e.db, "DELETE FROM "+records.TableNameFiles+" WHERE "+records.ColFileID+" = '"+g.ID+"'")
	}
	wantCode(t, e.store.SetVisibility(ctx, "t", g.ID, filesystem.VisibilityPublic), filesystem.CodeNotFound)
}

// TestQuarantineFaults fails each database step of the quarantine's
// operations in turn: every one surfaces, and none leaves a half change.
func TestQuarantineFaults(t *testing.T) {
	ctx := t.Context()
	db, faults := testdb.OpenFaulty(t)
	e := newEnvOn(t, db, nil, records.WithScanFailure(records.ScanFailurePurge))
	for name, op := range map[string]func(f records.File) error{
		"PassScan": func(f records.File) error { _, err := e.store.PassScan(ctx, "t", f.ID); return err },
		"FailScan": func(f records.File) error { _, err := e.store.FailScan(ctx, "t", f.ID, "infected: x"); return err },
		"Served": func(f records.File) error {
			return e.store.Served(ctx, records.Served{Kind: records.ServeStream, Scope: "t", FileID: f.ID})
		},
	} {
		f := e.quarantined(t, name+".png")
		faults.Arm(0)
		_ = op(f)
		steps := faults.Steps()
		for i := 1; i <= steps; i++ {
			g := e.quarantined(t, name+"-fault.png")
			faults.Arm(i)
			err := op(g)
			faults.Arm(0)
			if !errors.Is(err, testdb.ErrInjected) {
				t.Fatalf("%s step %d/%d = %v", name, i, steps, err)
			}
			if read, err := e.store.File(ctx, "t", g.ID); err != nil || !read.Quarantined() {
				t.Fatalf("%s step %d left %+v, %v", name, i, read, err)
			}
		}
	}
	// FailScan's keep path writes the detail: fail its update too.
	k := newEnvOn(t, db, nil)
	f := k.quarantined(t, "keep.png")
	faults.Arm(0)
	if _, err := k.store.FailScan(ctx, "t", f.ID, "infected: y"); err != nil {
		t.Fatal(err)
	}
	steps := faults.Steps()
	for i := 1; i <= steps; i++ {
		g := k.quarantined(t, "keep-fault.png")
		faults.Arm(i)
		_, err := k.store.FailScan(ctx, "t", g.ID, "infected: y")
		faults.Arm(0)
		if !errors.Is(err, testdb.ErrInjected) {
			t.Fatalf("FailScan keep step %d/%d = %v", i, steps, err)
		}
	}
}

// releasingHook tries to release a file by setting its status in a
// replacement, which the Store must ignore.
type releasingHook struct{ saw []records.Status }

func (*releasingHook) Name() string { return "releasing_hook" }
func (h *releasingHook) BeforeReplace(_ context.Context, _ *records.Store, _ records.File, next *records.File) error {
	h.saw = append(h.saw, next.Status)
	next.Status = records.StatusReady
	return nil
}

// TestQuarantineContent: a replacement with QuarantineContent quarantines
// the file with its new content; a quarantined file stays quarantined
// whatever replaces it and whatever a hook sets; a public file is refused
// and keeps its content; every other change refuses the option.
func TestQuarantineContent(t *testing.T) {
	ctx := t.Context()
	hook := &releasingHook{}
	e := newEnv(t, nil, records.WithPlugins(hook))
	f := e.put(t, "t", "a.png")
	got, err := e.store.Replace(ctx, "t", f.ID, strings.NewReader(pngData+"-v2"), "", images, records.QuarantineContent())
	if err != nil || !got.Quarantined() {
		t.Fatalf("Replace with QuarantineContent = %+v, %v", got, err)
	}
	// The hook is told the new content is quarantined, so it can skip it.
	if len(hook.saw) != 1 || hook.saw[0] != records.StatusQuarantined {
		t.Fatalf("the replace hook saw %v", hook.saw)
	}
	if read, _ := e.store.File(ctx, "t", f.ID); !read.Quarantined() || read.Key != got.Key {
		t.Fatalf("read back %+v", read)
	}
	got, err = e.store.Replace(ctx, "t", f.ID, strings.NewReader(pngData+"-v3"), "", images)
	if err != nil || !got.Quarantined() {
		t.Fatalf("a replacement released a quarantined file: %+v, %v", got, err)
	}
	if read, _ := e.store.File(ctx, "t", f.ID); !read.Quarantined() {
		t.Fatal("the row was released")
	}

	pub := e.put(t, "t", "pub.png")
	if err := e.store.SetVisibility(ctx, "t", pub.ID, filesystem.VisibilityPublic); err != nil {
		t.Fatal(err)
	}
	before := e.objectCount(t)
	_, err = e.store.Replace(ctx, "t", pub.ID, strings.NewReader(pngData+"-x"), "", images, records.QuarantineContent())
	if !errors.Is(err, records.ErrPublicQuarantine) {
		t.Fatalf("quarantining a public file's content = %v", err)
	}
	if read, _ := e.store.File(ctx, "t", pub.ID); read.Key != pub.Key || read.Status != records.StatusReady {
		t.Fatalf("a refused replacement changed the file: %+v", read)
	}
	if n := e.objectCount(t); n != before {
		t.Fatalf("a refused replacement left %d objects", n-before)
	}

	g := e.put(t, "t", "g.png")
	q := records.QuarantineContent()
	for name, err := range map[string]error{
		"Trash":         e.store.Trash(ctx, "t", g.ID, q),
		"Restore":       e.store.Restore(ctx, "t", g.ID, q),
		"Purge":         e.store.Purge(ctx, "t", g.ID, q),
		"SetVisibility": e.store.SetVisibility(ctx, "t", g.ID, filesystem.VisibilityPublic, q),
	} {
		if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || !strings.Contains(err.Error(), "QuarantineContent") {
			t.Fatalf("%s with QuarantineContent = %v", name, err)
		}
	}
	if read, _ := e.store.File(ctx, "t", g.ID); read.DeletedAt != nil || read.Visibility != filesystem.VisibilityPrivate {
		t.Fatalf("a refused change changed the file: %+v", read)
	}
}
