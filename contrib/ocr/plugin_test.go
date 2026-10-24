package ocr_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/ocr"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/upload"
)

const scope = "acme"

var imagePolicy = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*", "text/plain"}}

func newStore(t *testing.T, p *ocr.Plugin) (*records.Store, *filesystem.Disk) {
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
	return store, disk
}

func exists(t *testing.T, d *filesystem.Disk, key string) bool {
	t.Helper()
	ok, err := d.Exists(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// TestPlugin_StoresTheText runs the plugin with a stand-in tesseract that
// prints its own counter, so each recognition is told apart.
func TestPlugin_StoresTheText(t *testing.T) {
	counter := t.TempDir() + "/n"
	prog := fakeTesseract(t, `n=$(cat `+counter+` 2>/dev/null || echo 0); n=$((n+1)); echo $n > `+counter+`; echo "receipt $n"`)
	store, disk := newStore(t, ocr.NewPlugin(ocr.Tool{Tesseract: prog}))
	ctx := t.Context()
	f, err := store.Upload(ctx, scope, "media", bytes.NewReader(smallPNG(t, 8, 8)), "r.png", imagePolicy)
	if err != nil {
		t.Fatal(err)
	}
	f = reread(t, store, f)
	if f.Status != records.StatusReady {
		t.Fatalf("status = %q %q", f.Status, f.StatusDetail)
	}
	key, ok := ocr.TextKey(f)
	if !ok || !strings.HasPrefix(key, f.Key+ocr.TextInfix) {
		t.Fatalf("metadata = %v", f.Metadata)
	}
	if body, err := disk.ReadAll(ctx, key); err != nil || string(body) != "receipt 1" {
		t.Fatalf("text = %q, %v", body, err)
	}
	if st, err := disk.Stat(ctx, key); err != nil || st.ContentType != ocr.TextContentType {
		t.Fatalf("text object = %+v, %v", st, err)
	}
	next, err := store.Replace(ctx, scope, f.ID, bytes.NewReader(smallPNG(t, 9, 9)), "", imagePolicy)
	if err != nil {
		t.Fatal(err)
	}
	next = reread(t, store, next)
	nextKey, _ := ocr.TextKey(next)
	if nextKey == key || exists(t, disk, key) || !exists(t, disk, nextKey) {
		t.Fatalf("after replace: new %q, old exists %v", nextKey, exists(t, disk, key))
	}
	if err := store.Purge(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if exists(t, disk, nextKey) {
		t.Fatal("the text outlived its purged file")
	}

	// A rolled-back upload leaves no text behind.
	errAbort := errors.New("abort")
	err = store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := store.Upload(ctx, scope, "media", bytes.NewReader(smallPNG(t, 8, 8)), "r.png", imagePolicy); err != nil {
			return err
		}
		return errAbort
	})
	if !errors.Is(err, errAbort) {
		t.Fatal(err)
	}
	for _, err := range disk.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		t.Fatal("an object outlived the rolled-back upload")
	}
}

func TestPlugin_OtherFilesAndFailures(t *testing.T) {
	ctx := t.Context()
	// Other types are left alone, and the plugin's stale key is cleared.
	store, disk := newStore(t, ocr.NewPlugin(ocr.Tool{Tesseract: fakeTesseract(t, "echo never")}))
	in := records.NewFile{Scope: scope, Disk: "media", Key: "n.txt", Filename: "n.txt", ContentType: "text/plain", Metadata: map[string]string{ocr.MetaTextKey: "stale", "app": "kept"}}
	if _, err := disk.WriteBytes(ctx, in.Key, []byte("notes"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if f, err := store.CreateFile(ctx, in); err != nil || len(f.Metadata) != 1 || f.Metadata["app"] != "kept" {
		t.Fatalf("text file = %v, %v", f.Metadata, err)
	}
	// An image without text, and one tesseract cannot read, are recorded
	// without text, ready.
	for _, script := range []string{"true", "echo bad >&2; exit 1"} {
		store, _ := newStore(t, ocr.NewPlugin(ocr.Tool{Tesseract: fakeTesseract(t, script)}))
		f, err := store.Upload(ctx, scope, "media", bytes.NewReader(smallPNG(t, 8, 8)), "r.png", imagePolicy)
		if err != nil {
			t.Fatal(err)
		}
		if f = reread(t, store, f); f.Metadata != nil || f.Status != records.StatusReady {
			t.Fatalf("%s: %v %q", script, f.Metadata, f.Status)
		}
	}
	// A missing program leaves the upload recorded, marked failed, and a
	// retry once it is installed reads the image.
	p := ocr.NewPlugin(ocr.Tool{Tesseract: "/nonexistent/tesseract"})
	store, _ = newStore(t, p)
	f, err := store.Upload(ctx, scope, "media", bytes.NewReader(smallPNG(t, 8, 8)), "r.png", imagePolicy)
	if err != nil {
		t.Fatalf("a missing program failed the upload: %v", err)
	}
	if f = reread(t, store, f); f.Status != records.StatusFailed || f.StatusDetail != ocr.StatusFailedPrefix+string(filesystem.CodeUnavailable) {
		t.Fatalf("status = %q %q", f.Status, f.StatusDetail)
	}
	if err := p.Process(ctx, store, scope, f.ID); !errors.Is(err, ocr.ErrNoProgram) {
		t.Fatalf("a retry without the program: %v", err)
	}
	fixed := ocr.NewPlugin(ocr.Tool{Tesseract: fakeTesseract(t, "echo found")})
	if err := fixed.Process(ctx, store, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if f = reread(t, store, f); f.Status != records.StatusReady || f.Metadata[ocr.MetaTextKey] == "" {
		t.Fatalf("after the retry: %q %v", f.Status, f.Metadata)
	}
	if p.Name() != ocr.PluginName {
		t.Fatal("name")
	}
	if err := p.Process(ctx, store, scope, "missing"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a missing file: %v", err)
	}
}

func reread(t *testing.T, s *records.Store, f records.File) records.File {
	t.Helper()
	got, err := s.File(t.Context(), f.Scope, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestPlugin_UploadNeverWaitsOnTheEngine pins that recognition runs
// after the upload's transaction commits: while the engine runs, the
// file is visible, marked processing, from outside any transaction, and
// another tenant's upload goes through. Inside the transaction the file
// would be invisible (or, on SQLite's one connection, every read would
// wait until its timeout).
func TestPlugin_UploadNeverWaitsOnTheEngine(t *testing.T) {
	dir := t.TempDir()
	started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
	prog := fakeTesseract(t, "touch "+started+"; while [ ! -e "+release+" ]; do sleep 0.05; done; echo receipt")
	store, _ := newStore(t, ocr.NewPlugin(ocr.Tool{Tesseract: prog}))
	done := make(chan error, 1)
	go func() {
		_, err := store.Upload(context.Background(), scope, "media", bytes.NewReader(smallPNG(t, 8, 8)), "r.png", imagePolicy)
		done <- err
	}()
	defer func() { _ = os.WriteFile(release, nil, 0o600) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the engine never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	page, err := store.Files(ctx, records.FileQuery{Scope: scope})
	if err != nil || len(page.Files) != 1 || page.Files[0].Status != records.StatusProcessing || page.Files[0].StatusDetail != ocr.StatusDetail {
		t.Fatalf("while the engine ran: %+v, %v", page.Files, err)
	}
	if _, err := store.Upload(ctx, "another-tenant", "media", strings.NewReader("notes"), "n.txt", imagePolicy); err != nil {
		t.Fatalf("another tenant waited on the engine: %v", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestProcess_ReplacedMeanwhileAndRerun pins that a run whose image was
// replaced while the engine ran records nothing and leaves no object, and
// that running Process again replaces the text instead of orphaning it.
func TestProcess_ReplacedMeanwhileAndRerun(t *testing.T) {
	dir := t.TempDir()
	counter, started, release := filepath.Join(dir, "n"), filepath.Join(dir, "started"), filepath.Join(dir, "release")
	prog := fakeTesseract(t, `n=$(cat `+counter+` 2>/dev/null || echo 0); n=$((n+1)); echo $n > `+counter+`
if [ $n -eq 1 ]; then touch `+started+`; while [ ! -e `+release+` ]; do sleep 0.05; done; fi
echo "run $n"`)
	p := ocr.NewPlugin(ocr.Tool{Tesseract: prog})
	store, disk := newStore(t, p)
	done := make(chan error, 1)
	go func() {
		_, err := store.Upload(context.Background(), scope, "media", bytes.NewReader(smallPNG(t, 8, 8)), "r.png", imagePolicy)
		done <- err
	}()
	defer func() { _ = os.WriteFile(release, nil, 0o600) }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the engine never started")
		}
	}
	page, err := store.Files(t.Context(), records.FileQuery{Scope: scope})
	if err != nil || len(page.Files) != 1 {
		t.Fatalf("files: %v %v", page.Files, err)
	}
	if _, err := store.Replace(t.Context(), scope, page.Files[0].ID, bytes.NewReader(smallPNG(t, 9, 9)), "", imagePolicy); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f := reread(t, store, page.Files[0])
	check := func(want string) {
		t.Helper()
		key, _ := ocr.TextKey(f)
		if text, err := disk.ReadAll(t.Context(), key); err != nil || string(text) != want {
			t.Fatalf("text = %q, %v; want %q", text, err, want)
		}
		n := 0
		for _, err := range disk.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
			if err != nil {
				t.Fatal(err)
			}
			n++
		}
		if n != 2 {
			t.Fatalf("%d objects, want the image and its one text", n)
		}
	}
	check("run 2")
	if err := p.Process(t.Context(), store, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	f = reread(t, store, f)
	check("run 3")
}

// TestResume_FinishesWhatACrashLeft simulates a crash after the commit:
// the row is processing and nothing in memory will run it. A store
// without the plugin installed stands in for the restarted process.
func TestResume_FinishesWhatACrashLeft(t *testing.T) {
	ctx := t.Context()
	disk, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := filesystem.NewManager(disk)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := recordstest.Open(t)
	store, err := records.New(db, recordstest.Dialect(), mgr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := disk.WriteBytes(ctx, "r.png", smallPNG(t, 8, 8), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := store.CreateFile(ctx, records.NewFile{
		Scope: scope, Disk: "media", Key: "r.png", Filename: "r.png", ContentType: ocr.PNG,
		Status: records.StatusProcessing, StatusDetail: ocr.StatusDetail,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := ocr.NewPlugin(ocr.Tool{Tesseract: fakeTesseract(t, "echo receipt")})
	if n, err := p.Resume(ctx, store, f.CreatedAt.Add(-time.Minute)); err != nil || n != 0 {
		t.Fatalf("too recent: %d, %v", n, err)
	}
	if got := reread(t, store, f); got.Status != records.StatusProcessing {
		t.Fatalf("status = %q", got.Status)
	}
	if n, err := p.Resume(ctx, store, time.Now().Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("resume: %d, %v", n, err)
	}
	if got := reread(t, store, f); got.Status != records.StatusReady || got.Metadata[ocr.MetaTextKey] == "" {
		t.Fatalf("after resume: %q %v", got.Status, got.Metadata)
	}
	if n, err := p.Resume(ctx, store, time.Now().Add(time.Minute)); err != nil || n != 0 {
		t.Fatalf("second resume: %d, %v", n, err)
	}
}

// TestDeferredProcessing_UploadReturnsFirst pins that with
// WithDeferredProcessing an upload returns before tesseract runs, and
// Resume does the work.
func TestDeferredProcessing_UploadReturnsFirst(t *testing.T) {
	ran := filepath.Join(t.TempDir(), "ran")
	p := ocr.NewPlugin(ocr.Tool{Tesseract: fakeTesseract(t, "touch "+ran+"; echo receipt")}, ocr.WithDeferredProcessing())
	store, _ := newStore(t, p)
	f, err := store.Upload(t.Context(), scope, "media", bytes.NewReader(smallPNG(t, 8, 8)), "r.png", imagePolicy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("tesseract ran before the upload returned")
	}
	if f = reread(t, store, f); f.Status != records.StatusProcessing {
		t.Fatalf("status = %q", f.Status)
	}
	if n, err := p.Resume(t.Context(), store, time.Now().Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("resume: %d, %v", n, err)
	}
	if f = reread(t, store, f); f.Status != records.StatusReady || f.Metadata[ocr.MetaTextKey] == "" {
		t.Fatalf("after resume: %q %v", f.Status, f.Metadata)
	}
}

// TestPlugin_ProcessTimeout pins WithProcessTimeout: a recognition that
// outlives the bound is cut short, Process returns its error rather than
// waiting the engine out, and the file is marked failed.
func TestPlugin_ProcessTimeout(t *testing.T) {
	const bound = 200 * time.Millisecond
	// stuck is far past the bound, so finishing near the bound proves the
	// cut and not the engine ending on its own.
	const stuck = 20 * time.Second
	p := ocr.NewPlugin(ocr.Tool{Tesseract: fakeTesseract(t, "exec sleep 20")}, ocr.WithProcessTimeout(bound), ocr.WithDeferredProcessing())
	store, _ := newStore(t, p)
	f, err := store.Upload(t.Context(), scope, "media", bytes.NewReader(smallPNG(t, 8, 8)), "r.png", imagePolicy)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := p.Process(t.Context(), store, scope, f.ID); err == nil {
		t.Fatal("a stuck engine finished without an error")
	}
	if took := time.Since(start); took >= stuck/2 {
		t.Fatalf("Process waited %v for the engine; the bound is %v", took, bound)
	}
	if f = reread(t, store, f); f.Status != records.StatusFailed {
		t.Fatalf("status = %q %q, want failed", f.Status, f.StatusDetail)
	}
}
