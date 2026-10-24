package video_test

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/sqlb"
)

// errLocation is the failure of a Location the tests switch on.
var errLocation = errors.New("location service down")

// switchedLocation is a Location answering by what the test sets: an
// error, a disk name, or the file's own disk under "hls/".
type switchedLocation struct {
	err  error
	disk string
}

func (l *switchedLocation) locate(f records.File, _ string) (string, string, error) {
	if l.err != nil {
		return "", "", l.err
	}
	return l.disk, "hls/" + f.ID + "/", nil
}

// faultyJobEnv is jobEnv over a database whose steps can be made to fail.
func faultyJobEnv(t *testing.T, opts ...video.PluginOption) (*env, *recordstest.Faults, *video.Plugin) {
	t.Helper()
	tool := video.Tool{FFprobe: hlsProbe(t, false), FFmpeg: hlsFFmpeg(t, filepath.Join(t.TempDir(), "args"), []string{"720p", "480p", "360p"}, "")}
	p := video.NewPlugin(tool, opts...)
	media, _ := filesystem.NewDisk("media", memory.New())
	mgr, err := filesystem.NewManager(media)
	if err != nil {
		t.Fatal(err)
	}
	db, faults := recordstest.OpenFaulty(t)
	store, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &env{disk: media, store: store}, faults, p
}

// TestAttachHLS_LocationFailures: a Location error is returned as is by
// NewHLSLadder, EncodeHLS and AttachHLS, and a Location naming a disk
// the Manager lacks fails Writer, EncodeHLS and AttachHLS with that
// lookup's error; nothing is recorded.
func TestAttachHLS_LocationFailures(t *testing.T) {
	loc := &switchedLocation{}
	e, _, p := jobEnv(t, false, video.WithLocation(loc.locate))
	ctx := t.Context()
	f, err := e.upload(t, fakeMP4)
	if err != nil {
		t.Fatal(err)
	}
	good, err := p.NewHLSLadder(f)
	if err != nil || good.Disk != "media" {
		t.Fatalf("NewHLSLadder = %+v, %v", good, err)
	}
	writeLadder(t, e.store, good)

	loc.err = errLocation
	if _, err := p.NewHLSLadder(f); !errors.Is(err, errLocation) {
		t.Errorf("NewHLSLadder = %v", err)
	}
	if _, err := p.EncodeHLS(ctx, e.store, scope, f.ID, video.HLS{}); !errors.Is(err, errLocation) {
		t.Errorf("EncodeHLS = %v", err)
	}
	if _, err := p.AttachHLS(ctx, e.store, scope, f.ID, good); !errors.Is(err, errLocation) {
		t.Errorf("AttachHLS = %v", err)
	}

	loc.err, loc.disk = nil, "gone"
	nowhere, err := p.NewHLSLadder(f)
	if err != nil || nowhere.Disk != "gone" {
		t.Fatalf("NewHLSLadder on an unknown disk = %+v, %v", nowhere, err)
	}
	if _, err := nowhere.Writer(e.store); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("Writer on an unknown disk = %v", err)
	}
	if _, err := p.EncodeHLS(ctx, e.store, scope, f.ID, video.HLS{}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("EncodeHLS onto an unknown disk = %v", err)
	}
	if _, err := p.AttachHLS(ctx, e.store, scope, f.ID, nowhere); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("AttachHLS on an unknown disk = %v", err)
	}
	if stored, _ := e.store.File(ctx, scope, f.ID); stored.Metadata[video.MetaHLSKey] != "" {
		t.Fatalf("a failed attach recorded %v", stored.Metadata)
	}
}

// TestEncodeHLS_Refusals: EncodeHLS refuses a missing file, a file that
// is not a video, a quarantined video, one on a disk the Manager lacks
// and one whose object is gone, each before any ladder is written.
func TestEncodeHLS_Refusals(t *testing.T) {
	e, _, p := jobEnv(t, false)
	ctx := t.Context()
	if _, err := p.EncodeHLS(ctx, e.store, scope, "missing", video.HLS{}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("a missing file: %v", err)
	}
	txt, err := e.store.Upload(ctx, scope, "media", bytes.NewReader([]byte("hello\n")), "a.txt", videoPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.EncodeHLS(ctx, e.store, scope, txt.ID, video.HLS{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("a text file: %v", err)
	}
	held, err := e.store.Upload(ctx, scope, "media", bytes.NewReader(fakeMP4), "held.mp4", videoPolicy, records.Quarantine())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.EncodeHLS(ctx, e.store, scope, held.ID, video.HLS{}); filesystem.CodeOf(err) != filesystem.CodeConflict {
		t.Errorf("a quarantined video: %v", err)
	}
	gone, err := e.upload(t, fakeMP4)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.disk.Delete(ctx, gone.Key); err != nil {
		t.Fatal(err)
	}
	before := keysOn(t, e.disk)
	if _, err := p.EncodeHLS(ctx, e.store, scope, gone.ID, video.HLS{}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("a video whose object is gone: %v", err)
	}
	moved, err := e.upload(t, fakeMP4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Exec(ctx, sqlb.NewUpdate(e.store.Dialect()).Table(records.TableFiles).Set(sqlb.Set(records.FileDisk, "retired")).
		Where(sqlb.Eq(records.FileID, moved.ID))); err != nil {
		t.Fatal(err)
	}
	if _, err := p.EncodeHLS(ctx, e.store, scope, moved.ID, video.HLS{}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("a video on an unknown disk: %v", err)
	}
	after := keysOn(t, e.disk)
	if len(after) != len(before)+1 { // the second upload, and no ladder
		t.Fatalf("refused encodes left %v (before %v)", after, before)
	}
}

// TestAttachHLS_MissingFileAndNoMetadata: attaching to a file that does
// not exist is CodeNotFound, and a video whose row holds no metadata
// gets one naming the ladder.
func TestAttachHLS_MissingFileAndNoMetadata(t *testing.T) {
	e, _, p := jobEnv(t, false)
	ctx := t.Context()
	f, err := e.upload(t, fakeMP4)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := p.NewHLSLadder(f)
	writeLadder(t, e.store, l)
	if _, err := p.AttachHLS(ctx, e.store, scope, "missing", l); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("a missing file: %v", err)
	}
	if _, err := e.store.Exec(ctx, sqlb.NewUpdate(e.store.Dialect()).Table(records.TableFiles).Set(sqlb.Set(records.FileMetadata, "{}")).
		Where(sqlb.Eq(records.FileID, f.ID))); err != nil {
		t.Fatal(err)
	}
	got, err := p.AttachHLS(ctx, e.store, scope, f.ID, l)
	if err != nil {
		t.Fatal(err)
	}
	if want := l.Dir + video.HLSMaster; len(got.Metadata) != 1 || got.Metadata[video.MetaHLSKey] != want {
		t.Fatalf("metadata %v, want only %s=%s", got.Metadata, video.MetaHLSKey, want)
	}
}

// TestAttachHLS_DatabaseFailures: AttachHLS failing at any database step
// returns the failure, records nothing and deletes the ladder it was
// handed (the rollback's cleanup), as an inline ladder's failure does.
func TestAttachHLS_DatabaseFailures(t *testing.T) {
	e, faults, p := faultyJobEnv(t)
	ctx := t.Context()
	f, err := e.upload(t, fakeMP4)
	if err != nil {
		t.Fatal(err)
	}
	probe, _ := p.NewHLSLadder(f)
	writeLadder(t, e.store, probe)
	faults.Arm(0)
	if _, err := p.AttachHLS(ctx, e.store, scope, f.ID, probe); err != nil {
		t.Fatal(err)
	}
	steps := faults.Steps()
	f, _ = e.store.File(ctx, scope, f.ID)
	for n := 1; n <= steps; n++ {
		l, _ := p.NewHLSLadder(f)
		writeLadder(t, e.store, l)
		faults.Arm(n)
		_, err := p.AttachHLS(ctx, e.store, scope, f.ID, l)
		faults.Arm(0)
		if !errors.Is(err, recordstest.ErrInjected) {
			t.Fatalf("AttachHLS failing at step %d of %d = %v", n, steps, err)
		}
		stored, err := e.store.File(ctx, scope, f.ID)
		if err != nil || stored.Metadata[video.MetaHLSKey] != probe.Dir+video.HLSMaster {
			t.Fatalf("step %d: the row names %v, %v", n, stored.Metadata, err)
		}
	}
}
