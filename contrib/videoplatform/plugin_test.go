package videoplatform_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/videoplatform"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// fakePlatform is a second Platform implementation, in memory: it shows
// the plugin works with any platform, and lets tests drive webhooks. A
// webhook is authentic when its X-Fake-Secret header is "ok"; its body is
// "<asset id> <status> [detail]".
type fakePlatform struct {
	mu        sync.Mutex
	assets    map[string]videoplatform.Asset
	deleted   []string
	sources   []videoplatform.Source
	failNext  error
	nextIndex int
	// onIngest, when set, runs after Ingest creates an asset.
	onIngest func()
}

func newFake() *fakePlatform { return &fakePlatform{assets: map[string]videoplatform.Asset{}} }

func (f *fakePlatform) Name() string { return "fake" }

func (f *fakePlatform) Ingest(_ context.Context, src videoplatform.Source) (videoplatform.Asset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failNext; err != nil {
		f.failNext = nil
		return videoplatform.Asset{}, err
	}
	f.nextIndex++
	a := videoplatform.Asset{ID: fmt.Sprintf("asset-%d", f.nextIndex), Status: videoplatform.StatusPreparing, Passthrough: src.Passthrough}
	f.assets[a.ID] = a
	f.sources = append(f.sources, src)
	if f.onIngest != nil {
		f.onIngest()
	}
	return a, nil
}

func (f *fakePlatform) Asset(_ context.Context, id string) (videoplatform.Asset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.assets[id]
	if !ok {
		return videoplatform.Asset{}, filesystem.Errorf(filesystem.CodeNotFound, "fake: no asset %q", id)
	}
	return a, nil
}

func (f *fakePlatform) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.assets, id)
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakePlatform) Webhook(r *http.Request, body []byte) (videoplatform.Asset, error) {
	if r.Header.Get("X-Fake-Secret") != "ok" {
		return videoplatform.Asset{}, filesystem.Errorf(filesystem.CodePermission, "fake: bad secret")
	}
	fields := strings.SplitN(string(body), " ", 3)
	if len(fields) < 2 {
		return videoplatform.Asset{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "fake: unreadable webhook")
	}
	if fields[0] == "upload" {
		return videoplatform.Asset{}, videoplatform.ErrIgnoredEvent
	}
	// "asset-1@100 ready" dates the state at Unix second 100, as a real
	// platform's event time does.
	id, at, dated := strings.Cut(fields[0], "@")
	a := videoplatform.Asset{ID: id, Status: videoplatform.Status(fields[1])}
	if dated {
		sec, err := strconv.ParseInt(at, 10, 64)
		if err != nil {
			return videoplatform.Asset{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "fake: bad time")
		}
		a.At = time.Unix(sec, 0)
	}
	if len(fields) == 3 {
		a.Detail = fields[2]
	}
	if a.Status == videoplatform.StatusReady {
		a.HLS, a.Duration = "https://stream.example/"+a.ID+".m3u8", 2500*time.Millisecond
	}
	return a, nil
}

// set changes an asset as the platform would.
func (f *fakePlatform) set(a videoplatform.Asset) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assets[a.ID] = a
}

// recorder is a records plugin that keeps every event.
type recorder struct {
	mu     sync.Mutex
	events []records.Event
}

func (r *recorder) Name() string { return "recorder" }

func (r *recorder) OnEvent(_ context.Context, _ *records.Store, e records.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

// kinds lists the plugin's event kinds seen so far.
func (r *recorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		if strings.HasPrefix(string(e.Kind), videoplatform.PluginName+".") {
			out = append(out, string(e.Kind)+" "+e.Detail[videoplatform.DetailAssetID])
		}
	}
	return out
}

const scope = "acme"

type env struct {
	db       *sql.DB
	store    *records.Store
	fake     *fakePlatform
	plugin   *videoplatform.Plugin
	recorder *recorder
	hook     http.Handler
}

var policy = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"video/*", "text/plain"}}

// sourceURL stands in for a signed URL: the memory disk cannot sign.
func sourceURL(_ context.Context, _ *records.Store, f records.File) (string, error) {
	return "https://files.example/" + f.Key + "?sig=x", nil
}

func newEnv(t *testing.T, opts ...videoplatform.Option) *env {
	t.Helper()
	return newEnvOn(t, nil, append([]videoplatform.Option{videoplatform.WithSourceURL(sourceURL)}, opts...)...)
}

// newEnvOn is newEnv with disk options and without the stand-in source
// URL.
func newEnvOn(t *testing.T, diskOpts []filesystem.Option, opts ...videoplatform.Option) *env {
	t.Helper()
	disk, err := filesystem.NewDisk("media", memory.New(), diskOpts...)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := filesystem.NewManager(disk)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{fake: newFake(), recorder: &recorder{}}
	e.plugin = videoplatform.New(e.fake, opts...)
	e.db, _ = recordstest.Open(t)
	e.store, err = records.New(e.db, recordstest.Dialect(), mgr, records.WithPlugins(e.plugin, e.recorder))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.hook = e.plugin.Webhook(e.store, nil)
	return e
}

// mp4 is enough of an MP4 for the type sniffer.
var mp4 = append([]byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"), make([]byte, 64)...)

func (e *env) video(t *testing.T) records.File {
	t.Helper()
	f, err := e.store.Upload(t.Context(), scope, "media", bytes.NewReader(mp4), "clip.mp4", policy)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// post sends a webhook and returns the response status.
func (e *env) post(t *testing.T, body, secret string) int {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/hooks/video", strings.NewReader(body))
	r.Header.Set("X-Fake-Secret", secret)
	w := httptest.NewRecorder()
	e.hook.ServeHTTP(w, r)
	return w.Code
}

func (e *env) status(t *testing.T, id string) (records.Status, string) {
	t.Helper()
	f, err := e.store.File(t.Context(), scope, id)
	if err != nil {
		t.Fatal(err)
	}
	return f.Status, f.StatusDetail
}

func TestPublishAndWebhooks(t *testing.T) {
	e := newEnv(t)
	f := e.video(t)
	v, err := e.plugin.Publish(t.Context(), e.store, scope, f.ID)
	if err != nil || v.Asset.ID != "asset-1" || v.Asset.Status != videoplatform.StatusPreparing || v.Platform != "fake" {
		t.Fatalf("Publish = %+v, %v", v, err)
	}
	if src := e.fake.sources[0]; src.Passthrough != scope+"/"+f.ID || !strings.Contains(src.URL, f.Key) {
		t.Fatalf("source = %+v", src)
	}
	if st, _ := e.status(t, f.ID); st != records.StatusProcessing {
		t.Fatalf("status after Publish = %s", st)
	}
	if _, err := e.plugin.Publish(t.Context(), e.store, scope, f.ID); filesystem.CodeOf(err) != filesystem.CodeExists {
		t.Fatalf("second Publish: %v", err)
	}

	if code := e.post(t, "asset-1 ready", "ok"); code != http.StatusNoContent {
		t.Fatalf("ready webhook = %d", code)
	}
	got, err := e.plugin.Asset(t.Context(), e.store, scope, f.ID)
	if err != nil || got.Asset.Status != videoplatform.StatusReady || got.Asset.HLS != "https://stream.example/asset-1.m3u8" || got.Asset.Duration != 2500*time.Millisecond {
		t.Fatalf("after ready = %+v, %v", got, err)
	}
	if st, _ := e.status(t, f.ID); st != records.StatusReady {
		t.Fatalf("status after ready = %s", st)
	}
	// A late "preparing" (webhooks out of order) changes nothing; a
	// repeated "ready" (a replay of the same signed request) emits
	// nothing more.
	e.post(t, "asset-1 preparing", "ok")
	e.post(t, "asset-1 ready", "ok")
	if got, _ := e.plugin.Asset(t.Context(), e.store, scope, f.ID); got.Asset.Status != videoplatform.StatusReady {
		t.Fatalf("a late preparing regressed the asset: %+v", got)
	}
	want := []string{"videoplatform.published asset-1", "videoplatform.ready asset-1"}
	if fmt.Sprint(e.recorder.kinds()) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", e.recorder.kinds(), want)
	}
}

// TestWebhook_ReplayAfterALaterStateChangesNothing pins monotonic
// states: ready at t1, errored at t2, then the ready of t1 replayed (or
// delivered late) leaves the asset errored and emits nothing more; a
// later ready applies, and an undated Sync keeps the recorded date.
func TestWebhook_ReplayAfterALaterStateChangesNothing(t *testing.T) {
	e := newEnv(t)
	f := e.video(t)
	if _, err := e.plugin.Publish(t.Context(), e.store, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"asset-1@100 ready", "asset-1@200 errored broke", "asset-1@100 ready", "asset-1@200 ready"} {
		if code := e.post(t, body, "ok"); code != http.StatusNoContent {
			t.Fatalf("%s = %d", body, code)
		}
	}
	got, err := e.plugin.Asset(t.Context(), e.store, scope, f.ID)
	if err != nil || got.Asset.Status != videoplatform.StatusErrored || !got.Asset.At.Equal(time.Unix(200, 0)) {
		t.Fatalf("after the replays = %+v, %v", got.Asset, err)
	}
	want := []string{"videoplatform.published asset-1", "videoplatform.ready asset-1", "videoplatform.failed asset-1"}
	if fmt.Sprint(e.recorder.kinds()) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", e.recorder.kinds(), want)
	}
	if e.post(t, "asset-1@300 ready", "ok"); e.recorder.kinds()[len(e.recorder.kinds())-1] != "videoplatform.ready asset-1" {
		t.Fatalf("a later ready was dropped: %v", e.recorder.kinds())
	}
	e.fake.set(videoplatform.Asset{ID: "asset-1", Status: videoplatform.StatusReady, HLS: "https://h/y.m3u8"})
	if got, err := e.plugin.Sync(t.Context(), e.store, scope, f.ID); err != nil || got.Asset.HLS != "https://h/y.m3u8" || !got.Asset.At.Equal(time.Unix(300, 0)) {
		t.Fatalf("undated Sync = %+v, %v", got.Asset, err)
	}
	// Deleted on the platform: neither a ready replayed inside the
	// signature window, nor a later-dated one, nor an undated one revives it.
	before := len(e.recorder.kinds())
	e.post(t, "asset-1@400 deleted deleted on Mux", "ok")
	e.post(t, "asset-1@300 ready", "ok")
	e.post(t, "asset-1@500 ready", "ok")
	e.post(t, "asset-1 ready", "ok")
	got, err = e.plugin.Asset(t.Context(), e.store, scope, f.ID)
	if err != nil || got.Asset.Status != videoplatform.StatusDeleted || got.Asset.Detail != "deleted on Mux" {
		t.Fatalf("a replayed ready revived a deleted asset: %+v, %v", got.Asset, err)
	}
	if k := e.recorder.kinds(); len(k) != before+1 || k[len(k)-1] != "videoplatform.failed asset-1" {
		t.Fatalf("events after deletion = %v", k[before:])
	}
	if st, _ := e.status(t, f.ID); st != records.StatusFailed {
		t.Fatalf("file status after deletion and replay = %s", st)
	}
}

func TestWebhook_ErroredMarksTheFileFailed(t *testing.T) {
	e := newEnv(t)
	f := e.video(t)
	if _, err := e.plugin.Publish(t.Context(), e.store, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	e.post(t, "asset-1 errored invalid_input: no video track\x00", "ok")
	st, detail := e.status(t, f.ID)
	if st != records.StatusFailed || detail != "video platform: invalid_input: no video track" {
		t.Fatalf("status = %s %q", st, detail)
	}
	if k := e.recorder.kinds(); k[len(k)-1] != "videoplatform.failed asset-1" {
		t.Fatalf("events = %v", k)
	}
}

func TestWebhook_Refusals(t *testing.T) {
	e := newEnv(t)
	f := e.video(t)
	if _, err := e.plugin.Publish(t.Context(), e.store, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		body, secret string
		want         int
	}{
		"bad signature":         {"asset-1 ready", "forged", http.StatusForbidden},
		"signed but unreadable": {"garbage", "ok", http.StatusBadRequest},
		"not about an asset":    {"upload x", "ok", http.StatusNoContent},
		"an asset nobody owns":  {"asset-99 ready", "ok", http.StatusNoContent},
		"too large":             {strings.Repeat("x", videoplatform.MaxWebhookBytes+1), "ok", http.StatusRequestEntityTooLarge},
	} {
		if got := e.post(t, tc.body, tc.secret); got != tc.want {
			t.Errorf("%s: %d, want %d", name, got, tc.want)
		}
	}
	if st, _ := e.status(t, f.ID); st != records.StatusProcessing {
		t.Fatalf("a refused webhook changed the file: %s", st)
	}
	w := httptest.NewRecorder()
	e.hook.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/hooks/video", nil))
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET = %d", w.Code)
	}
}

// TestWebhook_DatabaseFailureAsksForARetry pins the 503: a webhook that
// could not be recorded must make the platform send it again.
func TestWebhook_DatabaseFailureAsksForARetry(t *testing.T) {
	e := newEnv(t)
	f := e.video(t)
	if _, err := e.plugin.Publish(t.Context(), e.store, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Exec(t.Context(), rawSQL("DROP TABLE "+records.TableFiles.Name)); err != nil {
		t.Fatal(err)
	}
	if code := e.post(t, "asset-1 ready", "ok"); code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d", code)
	}
}

// rawSQL is a statement of literal SQL, for breaking the database.
type rawSQL string

func (r rawSQL) Build() (sqlb.Query, error) { return sqlb.Query{SQL: string(r)}, nil }

func TestSync(t *testing.T) {
	e := newEnv(t)
	f := e.video(t)
	if _, err := e.plugin.Publish(t.Context(), e.store, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	e.fake.set(videoplatform.Asset{ID: "asset-1", Status: videoplatform.StatusReady, PlaybackID: "pb", HLS: "https://h/x.m3u8"})
	v, err := e.plugin.Sync(t.Context(), e.store, scope, f.ID)
	if err != nil || v.Asset.Status != videoplatform.StatusReady || v.Asset.PlaybackID != "pb" {
		t.Fatalf("Sync = %+v, %v", v, err)
	}
	if _, err := e.plugin.Sync(t.Context(), e.store, scope, "absent"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("Sync of an unpublished file: %v", err)
	}
	e.fake.mu.Lock()
	delete(e.fake.assets, "asset-1")
	e.fake.mu.Unlock()
	if _, err := e.plugin.Sync(t.Context(), e.store, scope, f.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("Sync of an asset gone from the platform: %v", err)
	}
}

func TestPublish_Refusals(t *testing.T) {
	e := newEnv(t)
	txt, err := e.store.Upload(t.Context(), scope, "media", strings.NewReader("hello\n"), "a.txt", policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.plugin.Publish(t.Context(), e.store, scope, txt.ID); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a text file: %v", err)
	}
	if _, err := e.plugin.Publish(t.Context(), e.store, scope, "absent"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("an unknown file: %v", err)
	}
	f := e.video(t)
	e.fake.failNext = filesystem.Errorf(filesystem.CodeUnavailable, "fake: down")
	if _, err := e.plugin.Publish(t.Context(), e.store, scope, f.ID); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("platform down: %v", err)
	}
	if _, err := e.plugin.Asset(t.Context(), e.store, scope, f.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a failed Publish left a row: %v", err)
	}
	boom := errors.New("no signer")
	failing := newEnv(t, videoplatform.WithSourceURL(func(context.Context, *records.Store, records.File) (string, error) { return "", boom }))
	if _, err := failing.plugin.Publish(t.Context(), failing.store, scope, failing.video(t).ID); !errors.Is(err, boom) {
		t.Fatalf("source URL failure: %v", err)
	}
}

// TestPublish_RollbackDeletesTheAsset pins object first, row second: when
// the transaction recording the asset rolls back, the asset the platform
// created is deleted.
func TestPublish_RollbackDeletesTheAsset(t *testing.T) {
	e := newEnv(t)
	f := e.video(t)
	abort := errors.New("caller aborts")
	err := e.store.Tx(t.Context(), func(ctx context.Context, _ *sql.Tx) error {
		if _, err := e.plugin.Publish(ctx, e.store, scope, f.ID); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) || fmt.Sprint(e.fake.deleted) != "[asset-1]" {
		t.Fatalf("err = %v, deleted = %v", err, e.fake.deleted)
	}
}

// TestPublish_FailureAfterIngestDropsTheAsset pins the asset's drop when
// recording fails once the platform created it, so no asset is left
// unrecorded on the platform. Cancelled during Ingest, the request rolls
// back (SQLite) or never begins its transaction (Postgres, MySQL); with
// the database gone, the transaction never begins on any engine and
// there is no rollback, so Publish itself must delete the asset.
func TestPublish_FailureAfterIngestDropsTheAsset(t *testing.T) {
	e := newEnv(t)
	f := e.video(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e.fake.onIngest = cancel
	if _, err := e.plugin.Publish(ctx, e.store, scope, f.ID); err == nil {
		t.Fatal("a cancelled Publish succeeded")
	}
	if fmt.Sprint(e.fake.deleted) != "[asset-1]" {
		t.Fatalf("cancelled: deleted = %v, want the ingested asset", e.fake.deleted)
	}
	e.fake.onIngest = func() { _ = e.db.Close() }
	if _, err := e.plugin.Publish(t.Context(), e.store, scope, f.ID); err == nil {
		t.Fatal("a Publish without a database succeeded")
	}
	if fmt.Sprint(e.fake.deleted) != "[asset-1 asset-2]" {
		t.Fatalf("no database: deleted = %v, want both ingested assets", e.fake.deleted)
	}
}

// TestApply_TiesAndUndatedStates pins admit: on equal times the further
// state wins (errored over ready in the same millisecond), and an
// undated report (a webhook without a usable time, Sync on a platform
// that dates nothing) never leaves a terminal state and never re-emits
// the current one.
func TestApply_TiesAndUndatedStates(t *testing.T) {
	e := newEnv(t)
	f := e.video(t)
	if _, err := e.plugin.Publish(t.Context(), e.store, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	e.post(t, "asset-1@100 ready", "ok")
	e.post(t, "asset-1@100 errored broke", "ok")
	e.post(t, "asset-1@100 ready", "ok")
	if got, _ := e.plugin.Asset(t.Context(), e.store, scope, f.ID); got.Asset.Status != videoplatform.StatusErrored {
		t.Fatalf("an errored tied with a ready lost: %+v", got.Asset)
	}
	before := len(e.recorder.kinds())
	e.post(t, "asset-1 ready", "ok")
	e.fake.set(videoplatform.Asset{ID: "asset-1", Status: videoplatform.StatusReady, HLS: "https://h/z.m3u8"})
	if got, err := e.plugin.Sync(t.Context(), e.store, scope, f.ID); err != nil || got.Asset.Status != videoplatform.StatusErrored {
		t.Fatalf("an undated ready left errored: %+v, %v", got.Asset, err)
	}
	if st, _ := e.status(t, f.ID); st != records.StatusFailed {
		t.Fatalf("file status = %s", st)
	}
	e.fake.set(videoplatform.Asset{ID: "asset-1", Status: videoplatform.StatusErrored, Detail: "broke again"})
	if got, err := e.plugin.Sync(t.Context(), e.store, scope, f.ID); err != nil || got.Asset.Detail != "broke again" {
		t.Fatalf("an undated errored did not refresh the row: %+v, %v", got.Asset, err)
	}
	if k := e.recorder.kinds(); len(k) != before {
		t.Fatalf("undated reports emitted %v", k[before:])
	}
}

func TestPurgeReplaceAndUnpublishDeleteTheAsset(t *testing.T) {
	e := newEnv(t)
	purged, replaced, unpublished := e.video(t), e.video(t), e.video(t)
	for _, f := range []records.File{purged, replaced, unpublished} {
		if _, err := e.plugin.Publish(t.Context(), e.store, scope, f.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.store.Purge(t.Context(), scope, purged.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Replace(t.Context(), scope, replaced.ID, bytes.NewReader(mp4), "new.mp4", policy); err != nil {
		t.Fatal(err)
	}
	if err := e.plugin.Unpublish(t.Context(), e.store, scope, unpublished.ID); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(e.fake.deleted) != "[asset-1 asset-2 asset-3]" {
		t.Fatalf("deleted = %v", e.fake.deleted)
	}
	for _, f := range []records.File{replaced, unpublished} {
		if _, err := e.plugin.Asset(t.Context(), e.store, scope, f.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
			t.Fatalf("row left for %s: %v", f.ID, err)
		}
	}
	if err := e.plugin.Unpublish(t.Context(), e.store, scope, unpublished.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("Unpublish twice: %v", err)
	}
	// A file never published purges without a platform call.
	if err := e.store.Purge(t.Context(), scope, e.video(t).ID); err != nil || len(e.fake.deleted) != 3 {
		t.Fatalf("purge of an unpublished file: %v, %v", err, e.fake.deleted)
	}
}

func TestPurgeScope(t *testing.T) {
	e := newEnv(t)
	f := e.video(t)
	if _, err := e.plugin.Publish(t.Context(), e.store, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	// A row whose file row is already gone (written by an older version,
	// or by hand) is still swept.
	if err := e.plugin.PurgeScope(t.Context(), e.store, scope); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(e.fake.deleted) != "[asset-1]" {
		t.Fatalf("deleted = %v", e.fake.deleted)
	}
	if _, err := e.store.PurgeScope(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
}

func TestPlatformTextIsCleaned(t *testing.T) {
	e := newEnv(t)
	f := e.video(t)
	if _, err := e.plugin.Publish(t.Context(), e.store, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	e.fake.set(videoplatform.Asset{ID: "asset-1", Status: "exploded", HLS: strings.Repeat("h", 2000), Detail: "x"})
	v, err := e.plugin.Sync(t.Context(), e.store, scope, f.ID)
	if err != nil || v.Asset.Status != videoplatform.StatusErrored || v.Asset.Detail != "unknown status exploded" || len(v.Asset.HLS) != 1024 {
		t.Fatalf("Sync = %+v, %v", v.Asset.Status, err)
	}
	e.fake.set(videoplatform.Asset{ID: "asset-1", Status: videoplatform.StatusErrored, Detail: strings.Repeat("é", 300) + "\xff"})
	v, err = e.plugin.Sync(t.Context(), e.store, scope, f.ID)
	if err != nil || len([]rune(v.Asset.Detail)) != 255 {
		t.Fatalf("detail = %d runes, %v", len([]rune(v.Asset.Detail)), err)
	}
}

// TestDefaultSourceURL pins the default download URL: the disk's signed
// URL, expiring after the source TTL.
func TestDefaultSourceURL(t *testing.T) {
	signer, err := signedurl.New("https://files.example", []signedurl.Key{{ID: "k1", Secret: bytes.Repeat([]byte("s"), 32)}})
	if err != nil {
		t.Fatal(err)
	}
	e := newEnvOn(t, []filesystem.Option{filesystem.WithURLSigner(signer)}, videoplatform.WithSourceTTL(-1), videoplatform.WithSourceTTL(10*time.Minute))
	if _, err := e.plugin.Publish(t.Context(), e.store, scope, e.video(t).ID); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(e.fake.sources[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := strconv.ParseInt(u.Query().Get(signedurl.ParamExpires), 10, 64)
	if left := time.Until(time.Unix(exp, 0)); err != nil || left < 9*time.Minute || left > 10*time.Minute {
		t.Fatalf("the source URL expires in %s (%v); want the 10-minute TTL", left, err)
	}
	unsigned := newEnvOn(t, nil)
	if _, err := unsigned.plugin.Publish(t.Context(), unsigned.store, scope, unsigned.video(t).ID); err == nil {
		t.Fatal("a disk that cannot sign produced a source URL")
	}
}

func TestOptionsAndHooks(t *testing.T) {
	p := videoplatform.New(newFake())
	if p.Name() != videoplatform.PluginName || p.BeforeCreate(t.Context(), nil, nil) != nil || p.AfterCreate(t.Context(), nil, records.File{}) != nil {
		t.Fatal("hooks")
	}
	if ms, err := p.Migrations(records.EngineMySQL); err != nil || len(ms) != 2 || ms[0].Owner != videoplatform.PluginName || ms[1].Owner != videoplatform.PluginName {
		t.Fatalf("Migrations = %v, %v", ms, err)
	}
}

// TestDatabaseFailures pins that a failing plugin table is reported, not
// read as "never published".
func TestDatabaseFailures(t *testing.T) {
	e := newEnv(t)
	f := e.video(t)
	if _, err := e.store.Exec(t.Context(), rawSQL("DROP TABLE "+videoplatform.TableNameAssets)); err != nil {
		t.Fatal(err)
	}
	notMissing := func(name string, err error) {
		t.Helper()
		if err == nil || filesystem.CodeOf(err) == filesystem.CodeNotFound {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, err := e.plugin.Asset(t.Context(), e.store, scope, f.ID)
	notMissing("Asset", err)
	_, err = e.plugin.Publish(t.Context(), e.store, scope, f.ID)
	notMissing("Publish", err)
	notMissing("PurgeScope", e.plugin.PurgeScope(t.Context(), e.store, scope))
	notMissing("Purge", e.store.Purge(t.Context(), scope, f.ID))
	if code := e.post(t, "asset-1 ready", "ok"); code != http.StatusServiceUnavailable {
		t.Errorf("webhook = %d", code)
	}
	if len(e.fake.sources) != 0 {
		t.Fatal("Publish called the platform although it could not check for an existing asset")
	}
}
