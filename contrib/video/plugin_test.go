package video_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/upload"
)

const scope = "acme"

var videoPolicy = upload.Policy{MaxSize: 50 << 20, Allowed: []string{"video/*", "text/plain"}}

type env struct {
	disk  *filesystem.Disk
	store *records.Store
}

func newEnv(t *testing.T, p *video.Plugin) *env {
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
	return &env{disk: disk, store: store}
}

func (e *env) upload(t *testing.T, body []byte) (records.File, error) {
	t.Helper()
	return e.store.Upload(t.Context(), scope, "media", bytes.NewReader(body), "clip.mp4", videoPolicy)
}

func (e *env) exists(t *testing.T, key string) bool {
	t.Helper()
	ok, err := e.disk.Exists(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestPlugin_RecordsTheMetadataAndAPoster(t *testing.T) {
	body := clip(t)
	e := newEnv(t, video.NewPlugin(video.Tool{}, video.WithPoster(time.Second)))
	f, err := e.upload(t, body)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := video.Of(f)
	if !ok || m.Duration != clipSeconds*time.Second || m.Width != 320 || m.Height != 240 || m.VideoCodec != "h264" || m.AudioCodec != "aac" {
		t.Fatalf("meta = %+v %v (%v)", m, ok, f.Metadata)
	}
	poster, ok := video.PosterKey(f)
	if !ok || !strings.HasPrefix(poster, f.Key+video.PosterInfix) {
		t.Fatalf("poster key = %q", poster)
	}
	jpg, err := e.disk.ReadAll(t.Context(), poster)
	if err != nil || !bytes.HasPrefix(jpg, []byte{0xFF, 0xD8, 0xFF}) {
		t.Fatalf("poster = %d bytes, %v", len(jpg), err)
	}

	// A replacement gets its own poster; the old one goes after the commit.
	next, err := e.store.Replace(t.Context(), scope, f.ID, bytes.NewReader(rotated(t)), "", videoPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := video.Of(next); m.Width != 240 || m.Height != 320 {
		t.Fatalf("replaced meta = %+v", m)
	}
	nextPoster, _ := video.PosterKey(next)
	if nextPoster == poster || !e.exists(t, nextPoster) || e.exists(t, poster) {
		t.Fatalf("after replace: new %q exists %v, old %q exists %v", nextPoster, e.exists(t, nextPoster), poster, e.exists(t, poster))
	}

	// The poster goes with the file.
	if err := e.store.Purge(t.Context(), scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if e.exists(t, nextPoster) {
		t.Fatal("the poster outlived its purged file")
	}
}

func TestPlugin_RollbackDeletesThePoster(t *testing.T) {
	body := clip(t)
	e := newEnv(t, video.NewPlugin(video.Tool{}, video.WithPoster(0)))
	errAbort := errors.New("abort")
	err := e.store.Tx(t.Context(), func(ctx context.Context, _ *sql.Tx) error {
		if _, err := e.store.Upload(ctx, scope, "media", bytes.NewReader(body), "clip.mp4", videoPolicy); err != nil {
			return err
		}
		return errAbort
	})
	if !errors.Is(err, errAbort) {
		t.Fatal(err)
	}
	for _, err := range e.disk.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		t.Fatal("an object outlived the rolled-back upload")
	}
}

// TestPlugin_RollbackAfterCancelDeletesThePoster pins, through the
// plugin, the Store's promise that rollback work runs without the
// request's cancellation: a request cancelled mid-upload still loses its
// poster.
func TestPlugin_RollbackAfterCancelDeletesThePoster(t *testing.T) {
	body := clip(t)
	e := newEnv(t, video.NewPlugin(video.Tool{}, video.WithPoster(0)))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_ = e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := e.store.Upload(ctx, scope, "media", bytes.NewReader(body), "clip.mp4", videoPolicy); err != nil {
			return err
		}
		cancel()
		return ctx.Err()
	})
	for entry, err := range e.disk.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(entry.Key, video.PosterInfix) {
			t.Fatalf("the poster %s outlived the cancelled upload", entry.Key)
		}
	}
}

func TestPlugin_UnreadableAndOtherFiles(t *testing.T) {
	requirePrograms(t)
	// A text file is not a video: no metadata.
	e := newEnv(t, video.NewPlugin(video.Tool{}, video.WithPoster(0)))
	f, err := e.store.Upload(t.Context(), scope, "media", strings.NewReader("quarterly numbers\n"), "notes.txt", videoPolicy)
	if err != nil || f.Metadata != nil {
		t.Fatalf("text = %v, %v", f.Metadata, err)
	}
	// A file recorded as video/mp4 that ffprobe cannot read: recorded
	// without metadata, or refused with WithStrict.
	if _, err := e.disk.WriteBytes(t.Context(), "broken.mp4", []byte("not a video at all"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	in := records.NewFile{Scope: scope, Disk: "media", Key: "broken.mp4", Filename: "broken.mp4", ContentType: "video/mp4", Metadata: map[string]string{video.MetaWidth: "stale", "app": "kept"}}
	f, err = e.store.CreateFile(t.Context(), in)
	if err != nil || len(f.Metadata) != 1 || f.Metadata["app"] != "kept" {
		t.Fatalf("broken = %v, %v", f.Metadata, err)
	}
	strict := newEnv(t, video.NewPlugin(video.Tool{}, video.WithStrict()))
	if _, err := strict.disk.WriteBytes(t.Context(), "broken.mp4", []byte("not a video at all"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := strict.store.CreateFile(t.Context(), in); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("strict: %v", err)
	}
}

func TestPlugin_MissingProgramFailsLoudly(t *testing.T) {
	requirePrograms(t)
	body := clip(t)
	e := newEnv(t, video.NewPlugin(video.Tool{FFprobe: "/nonexistent/ffprobe"}))
	if _, err := e.upload(t, body); !errors.Is(err, video.ErrNoProgram) {
		t.Fatalf("err = %v, want ErrNoProgram", err)
	}
}

func TestOf_WithoutMetadata(t *testing.T) {
	if _, ok := video.Of(records.File{}); ok {
		t.Fatal("Of reported metadata for a file without any")
	}
	if _, ok := video.PosterKey(records.File{}); ok {
		t.Fatal("PosterKey reported a poster for a file without one")
	}
	if video.NewPlugin(video.Tool{}).Name() != video.PluginName {
		t.Fatal("name")
	}
}
