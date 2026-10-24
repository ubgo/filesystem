package video_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video"
	"github.com/ubgo/filesystem/records"
)

// jobEnv is a store whose plugin encodes no ladder inline (no WithHLS):
// ladders come from EncodeHLS or AttachHLS, with fake programs; failing
// makes the fake ffmpeg write a segment and fail.
func jobEnv(t *testing.T, failing bool, opts ...video.PluginOption) (*env, *filesystem.Disk, *video.Plugin) {
	t.Helper()
	extra := ""
	if failing {
		extra = "exit 1"
	}
	tool := video.Tool{FFprobe: hlsProbe(t, false), FFmpeg: hlsFFmpeg(t, filepath.Join(t.TempDir(), "args"), []string{"720p", "480p", "360p"}, extra)}
	p := video.NewPlugin(tool, opts...)
	e, cdn := twoDiskEnv(t, p)
	return e, cdn, p
}

// writeLadder writes a minimal ladder (a segment and the master) into l.
func writeLadder(t *testing.T, s *records.Store, l video.HLSLadder) {
	t.Helper()
	w, err := l.Writer(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"360p_00000.ts", video.HLSMaster} {
		if err := w(t.Context(), name, video.HLSPlaylistContentType, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
}

// TestEncodeHLS_FollowsInlineRules pins R-B2: a ladder made in a job
// (EncodeHLS, outside the upload) is recorded like an inline one and
// then follows the same rules: placed by the Location, kept by the sweep,
// moved with the file, deleted by a replacement and by the purge.
func TestEncodeHLS_FollowsInlineRules(t *testing.T) {
	e, cdn, p := jobEnv(t, false)
	f, err := e.upload(t, fakeMP4)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := video.HLSLocation(f); ok {
		t.Fatal("a ladder was encoded inline without WithHLS")
	}
	f, err = p.EncodeHLS(t.Context(), e.store, scope, f.ID, video.HLS{})
	if err != nil {
		t.Fatal(err)
	}
	disk, master, ok := video.HLSLocation(f)
	if !ok || disk != "media" || !strings.HasPrefix(master, f.Key+video.HLSInfix) || len(ladderKeys(t, e.disk, master)) != 10 {
		t.Fatalf("ladder at %q %q (%v)", disk, master, f.Metadata)
	}
	if stored, _ := e.store.File(t.Context(), scope, f.ID); stored.Metadata[video.MetaHLSKey] != master {
		t.Fatalf("the row does not name the ladder: %v", stored.Metadata)
	}
	if n := sweepAll(t, e.store, "media"); n != 0 {
		t.Fatalf("the sweep deleted %d files of an attached ladder", n)
	}
	if _, err := e.store.MoveFile(t.Context(), scope, f.ID, "cdn"); err != nil {
		t.Fatal(err)
	}
	if len(ladderKeys(t, cdn, master)) != 10 || len(ladderKeys(t, e.disk, master)) != 0 {
		t.Fatal("the attached ladder did not move with its file")
	}
	if _, err := e.store.Replace(t.Context(), scope, f.ID, strings.NewReader(string(append(fakeMP4, 1))), "clip.mp4", videoPolicy); err != nil {
		t.Fatal(err)
	}
	if got := ladderKeys(t, cdn, master); len(got) != 0 {
		t.Fatalf("a replacement left the attached ladder: %v", got)
	}
	f, err = p.EncodeHLS(t.Context(), e.store, scope, f.ID, video.HLS{})
	if err != nil {
		t.Fatal(err)
	}
	_, master, _ = video.HLSLocation(f)
	if err := e.store.Purge(t.Context(), scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if got := ladderKeys(t, cdn, master); len(got) != 0 {
		t.Fatalf("the purge left %v", got)
	}
}

// TestAttachHLS_ReplacesAndRepeats: attaching a second ladder deletes the
// first after the commit, and attaching the ladder the file already has
// changes nothing and deletes nothing.
func TestAttachHLS_ReplacesAndRepeats(t *testing.T) {
	e, _, p := jobEnv(t, false)
	f, err := e.upload(t, fakeMP4)
	if err != nil {
		t.Fatal(err)
	}
	first, err := p.NewHLSLadder(f)
	if err != nil {
		t.Fatal(err)
	}
	writeLadder(t, e.store, first)
	if f, err = p.AttachHLS(t.Context(), e.store, scope, f.ID, first); err != nil {
		t.Fatal(err)
	}
	if again, err := p.AttachHLS(t.Context(), e.store, scope, f.ID, first); err != nil || !e.exists(t, first.Dir+video.HLSMaster) || again.Metadata[video.MetaHLSKey] != first.Dir+video.HLSMaster {
		t.Fatalf("re-attaching the same ladder: %v, %v", err, again.Metadata)
	}
	second, _ := p.NewHLSLadder(f)
	writeLadder(t, e.store, second)
	if _, err := p.AttachHLS(t.Context(), e.store, scope, f.ID, second); err != nil {
		t.Fatal(err)
	}
	if e.exists(t, first.Dir+video.HLSMaster) || e.exists(t, first.Dir+"360p_00000.ts") || !e.exists(t, second.Dir+video.HLSMaster) {
		t.Fatal("the first ladder outlived the second's attach, or the second is gone")
	}
}

// TestAttachHLS_Refusals pins the invariants: a ladder outside the place
// the Location gives the file (another directory, another disk, the
// file's own directory) is refused and nothing there is deleted, since
// the purge would delete the whole directory; a missing master, a file
// replaced since its source was read, a file that is not a video and a
// trashed file are refused; and nothing is recorded in any case.
func TestAttachHLS_Refusals(t *testing.T) {
	e, cdn, p := jobEnv(t, false)
	f, err := e.upload(t, fakeMP4)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	good, _ := p.NewHLSLadder(f)
	dir := f.Key[:strings.LastIndex(f.Key, "/")+1]
	for name, l := range map[string]video.HLSLadder{
		"the file's directory": {Disk: "media", Dir: dir, SourceSHA256: f.SHA256},
		"the disk root":        {Disk: "media", Dir: "", SourceSHA256: f.SHA256},
		"a nested directory":   {Disk: "media", Dir: good.Dir + "x/", SourceSHA256: f.SHA256},
		"another disk":         {Disk: "cdn", Dir: good.Dir, SourceSHA256: f.SHA256},
		"traversal":            {Disk: "media", Dir: f.Key + video.HLSInfix + "../", SourceSHA256: f.SHA256},
	} {
		if _, err := p.AttachHLS(ctx, e.store, scope, f.ID, l); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !e.exists(t, f.Key) {
		t.Fatal("a refused attach deleted the video")
	}
	if _, err := p.AttachHLS(ctx, e.store, scope, f.ID, good); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("a ladder with no master: %v", err)
	}
	writeLadder(t, e.store, good)
	stale := good
	stale.SourceSHA256 = strings.Repeat("0", 64)
	if _, err := p.AttachHLS(ctx, e.store, scope, f.ID, stale); filesystem.CodeOf(err) != filesystem.CodeConflict {
		t.Errorf("a ladder of replaced content: %v", err)
	}
	txt, err := e.store.Upload(ctx, scope, "media", strings.NewReader("hello\n"), "a.txt", videoPolicy)
	if err != nil {
		t.Fatal(err)
	}
	tl, _ := p.NewHLSLadder(txt)
	if _, err := p.AttachHLS(ctx, e.store, scope, txt.ID, tl); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("a text file: %v", err)
	}
	if err := e.store.Trash(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AttachHLS(ctx, e.store, scope, f.ID, good); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("a trashed file: %v", err)
	}
	if stored, _ := e.store.File(ctx, scope, f.ID); stored.Metadata[video.MetaHLSKey] != "" {
		t.Fatalf("a refused attach recorded %v", stored.Metadata)
	}
	if !e.exists(t, good.Dir+video.HLSMaster) {
		t.Fatal("a refused attach deleted the ladder")
	}
	if keys := keysOn(t, cdn); len(keys) != 0 {
		t.Fatalf("cdn holds %v", keys)
	}
}

// TestAttachHLS_RollbackAndEncodeFailure: an attach whose transaction
// rolls back deletes the ladder and records nothing, as an inline one
// does; an EncodeHLS whose encode fails leaves no file of the ladder.
func TestAttachHLS_RollbackAndEncodeFailure(t *testing.T) {
	e, _, p := jobEnv(t, false)
	f, err := e.upload(t, fakeMP4)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := p.NewHLSLadder(f)
	writeLadder(t, e.store, l)
	abort := errors.New("caller aborts")
	err = e.store.Tx(t.Context(), func(ctx context.Context, _ *sql.Tx) error {
		if _, err := p.AttachHLS(ctx, e.store, scope, f.ID, l); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if e.exists(t, l.Dir+video.HLSMaster) {
		t.Fatal("a rolled-back attach left the ladder")
	}
	if stored, _ := e.store.File(t.Context(), scope, f.ID); stored.Metadata[video.MetaHLSKey] != "" {
		t.Fatalf("a rolled-back attach recorded %v", stored.Metadata)
	}

	fe, _, fp := jobEnv(t, true)
	ff, err := fe.upload(t, fakeMP4)
	if err != nil {
		t.Fatal(err)
	}
	before := keysOn(t, fe.disk)
	if _, err := fp.EncodeHLS(t.Context(), fe.store, scope, ff.ID, video.HLS{}); err == nil {
		t.Fatal("a failed encode was attached")
	}
	if after := keysOn(t, fe.disk); !slices.Equal(before, after) {
		t.Fatalf("a failed encode left %v (before %v)", after, before)
	}
}

// TestAttachHLS_RepeatInARolledBackTx: re-attaching the ladder a file
// already has, in a transaction that rolls back, leaves that live ladder
// alone (a rollback deletes only a ladder the attach recorded).
func TestAttachHLS_RepeatInARolledBackTx(t *testing.T) {
	e, _, p := jobEnv(t, false)
	f, err := e.upload(t, fakeMP4)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := p.NewHLSLadder(f)
	writeLadder(t, e.store, l)
	if _, err := p.AttachHLS(t.Context(), e.store, scope, f.ID, l); err != nil {
		t.Fatal(err)
	}
	abort := errors.New("caller aborts")
	err = e.store.Tx(t.Context(), func(ctx context.Context, _ *sql.Tx) error {
		if _, err := p.AttachHLS(ctx, e.store, scope, f.ID, l); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if !e.exists(t, l.Dir+video.HLSMaster) {
		t.Fatal("a rolled-back repeat attach deleted the live ladder")
	}
}

// TestAttachHLS_LocationDisk: with WithLocation, the ladder goes on the
// Location's disk, which is recorded in MetaHLSDisk, and the purge
// deletes it there; an EncodeHLS whose attach fails (here the Location
// moves between the encode and the attach) deletes what it wrote.
func TestAttachHLS_LocationDisk(t *testing.T) {
	calls := 0
	onCDN := func(f records.File, name string) (string, string, error) {
		calls++
		return "cdn", "hls/" + f.ID + "-", nil
	}
	e, cdn, p := jobEnv(t, false, video.WithLocation(onCDN))
	f, err := e.upload(t, fakeMP4)
	if err != nil {
		t.Fatal(err)
	}
	if f, err = p.EncodeHLS(t.Context(), e.store, scope, f.ID, video.HLS{}); err != nil {
		t.Fatal(err)
	}
	disk, master, ok := video.HLSLocation(f)
	if !ok || disk != "cdn" || f.Metadata[video.MetaHLSDisk] != "cdn" || len(ladderKeys(t, cdn, master)) != 10 {
		t.Fatalf("ladder at %q %q (%v)", disk, master, f.Metadata)
	}
	if err := e.store.Purge(t.Context(), scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if got := keysOn(t, cdn); len(got) != 0 {
		t.Fatalf("the purge left %v on cdn", got)
	}

	moving := func(f records.File, name string) (string, string, error) {
		calls++
		return "cdn", "hls/" + strconv.Itoa(calls) + "/", nil
	}
	me, mcdn, mp := jobEnv(t, false, video.WithLocation(moving))
	mf, err := me.upload(t, fakeMP4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mp.EncodeHLS(t.Context(), me.store, scope, mf.ID, video.HLS{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("an attach after the Location moved: %v", err)
	}
	if got := keysOn(t, mcdn); len(got) != 0 {
		t.Fatalf("a failed attach left %v", got)
	}
}
