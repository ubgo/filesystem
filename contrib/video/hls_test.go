package video_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// hlsProbe is a fake ffprobe reporting a 1280x720 H.264 video of 4
// seconds, with an audio track unless silent.
func hlsProbe(t *testing.T, silent bool) string {
	t.Helper()
	audio := `,{"codec_type":"audio","codec_name":"aac"}`
	if silent {
		audio = ""
	}
	rep := `{"format":{"format_name":"mov","duration":"4"},"streams":[{"codec_type":"video","codec_name":"h264","width":1280,"height":720}` + audio + `]}`
	return fakeProgram(t, "ffprobe", `echo '`+rep+`'`)
}

// hlsFFmpeg is a fake ffmpeg that logs its arguments to log, one per
// line, and writes a ladder for renditions into the directory of its
// output ($last is <dir>/%v.m3u8), then runs extra.
func hlsFFmpeg(t *testing.T, log string, renditions []string, extra string) string {
	t.Helper()
	script := `printf '%s\n' "$@" > '` + log + `'
d=$(dirname "$last")
for n in ` + strings.Join(renditions, " ") + `; do
  printf 'segment %s 0' "$n" > "$d/${n}_00000.ts"
  printf 'segment %s 1' "$n" > "$d/${n}_00001.ts"
  printf '#EXTM3U\n%s_00000.ts\n%s_00001.ts\n' "$n" "$n" > "$d/$n.m3u8"
done
printf '#EXTM3U\n' > "$d/master.m3u8"
` + extra
	return fakeProgram(t, "ffmpeg", script)
}

// written is what an HLSWriter received, in order.
type written struct {
	names, types []string
	bodies       map[string]string
}

func (w *written) write(_ context.Context, name, contentType string, body io.Reader) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if w.bodies == nil {
		w.bodies = map[string]string{}
	}
	w.names, w.types = append(w.names, name), append(w.types, contentType)
	w.bodies[name] = string(b)
	return nil
}

// TestHLS_WritesTheLadderInOrderWithItsBounds pins Tool.HLS with fake
// programs: the 720p source drops the 1080p rung (720p is the top), every
// file reaches the writer with its type, segments first and the master
// last, and FFmpeg's arguments keep Transcode's bounds (input whitelists,
// duration, frame rate and count, first streams, no metadata).
func TestHLS_WritesTheLadderInOrderWithItsBounds(t *testing.T) {
	log := filepath.Join(t.TempDir(), "args")
	ladder := []string{"720p", "480p", "360p"}
	tool := video.Tool{FFprobe: hlsProbe(t, false), FFmpeg: hlsFFmpeg(t, log, ladder, "")}
	var w written
	if err := tool.HLS(t.Context(), strings.NewReader("video"), video.HLS{MaxDuration: 10 * time.Second, MaxFrameRate: 24}, w.write); err != nil {
		t.Fatal(err)
	}
	want := []string{"360p_00000.ts", "360p_00001.ts", "480p_00000.ts", "480p_00001.ts", "720p_00000.ts", "720p_00001.ts", "360p.m3u8", "480p.m3u8", "720p.m3u8", video.HLSMaster}
	if !slices.Equal(w.names, want) {
		t.Fatalf("written %v\nwant    %v", w.names, want)
	}
	for i, name := range w.names {
		wantType := video.HLSSegmentContentType
		if strings.HasSuffix(name, ".m3u8") {
			wantType = video.HLSPlaylistContentType
		}
		if w.types[i] != wantType {
			t.Errorf("%s typed %q", name, w.types[i])
		}
	}
	if w.bodies["480p_00001.ts"] != "segment 480p 1" {
		t.Errorf("segment body = %q", w.bodies["480p_00001.ts"])
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	args := "\n" + string(b)
	for _, want := range []string{
		"\n-protocol_whitelist\nfile\n", "\n-t\n10.000\n", "\n-fpsmax\n24\n", "\n-frames:v\n241\n",
		"\n-map\n[v0]\n-map\n0:a:0\n", "\n-map_metadata\n-1\n-map_chapters\n-1\n", "\n-f\nhls\n",
		"\n-var_stream_map\nv:0,a:0,name:720p v:1,a:1,name:480p v:2,a:2,name:360p\n",
		"\n-b:v:0\n2800000\n-maxrate:v:0\n2800000\n",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("ffmpeg arguments lack %q:%s", want, args)
		}
	}
	if strings.Contains(args, "1080p") {
		t.Error("an upscaled 1080p rung was encoded")
	}
}

func TestHLS_SilentVideoMapsNoAudio(t *testing.T) {
	log := filepath.Join(t.TempDir(), "args")
	tool := video.Tool{FFprobe: hlsProbe(t, true), FFmpeg: hlsFFmpeg(t, log, []string{"small"}, "")}
	var w written
	opts := video.HLS{Renditions: []video.Rendition{{Name: "small", MaxWidth: 320, MaxHeight: 180, VideoBitRate: 300_000}}}
	if err := tool.HLS(t.Context(), strings.NewReader("video"), opts, w.write); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if args := string(b); strings.Contains(args, "0:a:0") || strings.Contains(args, "-c:a") || !strings.Contains(args, "v:0,name:small\n") {
		t.Fatalf("silent video arguments:\n%s", args)
	}
}

// TestHLS_RefusesWhatItDidNotAskFor pins the check of FFmpeg's output: a
// file the ladder does not name, a missing master or playlist, and output
// past MaxOutput fail, and nothing reaches the writer.
func TestHLS_RefusesWhatItDidNotAskFor(t *testing.T) {
	ladder := []string{"720p", "480p", "360p"}
	for name, tc := range map[string]struct {
		extra string
		opts  video.HLS
		code  filesystem.Code
	}{
		"a stray file":      {`echo x > "$d/run.sh"`, video.HLS{}, filesystem.CodeUnavailable},
		"a sub-directory":   {`mkdir "$d/sub"`, video.HLS{}, filesystem.CodeUnavailable},
		"no master":         {`rm "$d/master.m3u8"`, video.HLS{}, filesystem.CodeUnavailable},
		"no playlist":       {`rm "$d/480p.m3u8"`, video.HLS{}, filesystem.CodeUnavailable},
		"a foreign segment": {`echo x > "$d/1080p_00000.ts"`, video.HLS{}, filesystem.CodeUnavailable},
		"over MaxOutput":    {`head -c 4096 /dev/zero > "$d/720p_00002.ts"`, video.HLS{MaxOutput: 1024}, filesystem.CodeTooLarge},
		"grows while running": {`i=0; while [ $i -lt 50 ]; do head -c 65536 /dev/zero > "$d/720p_1$i.ts"; i=$((i+1)); sleep 0.1; done`,
			video.HLS{MaxOutput: 128 << 10}, filesystem.CodeTooLarge},
		"ffmpeg fails": {`exit 1`, video.HLS{}, filesystem.CodeRejected},
	} {
		tool := video.Tool{FFprobe: hlsProbe(t, false), FFmpeg: hlsFFmpeg(t, filepath.Join(t.TempDir(), "args"), ladder, tc.extra)}
		var w written
		start := time.Now()
		err := tool.HLS(t.Context(), strings.NewReader("video"), tc.opts, w.write)
		if filesystem.CodeOf(err) != tc.code || len(w.names) != 0 {
			t.Errorf("%s: err = %v, wrote %v", name, err, w.names)
		}
		if name == "grows while running" && time.Since(start) > 4*time.Second {
			t.Errorf("growing output was not stopped early (%s)", time.Since(start))
		}
	}
}

func TestHLS_OptionsAndWriterErrors(t *testing.T) {
	tool := video.Tool{FFprobe: hlsProbe(t, false), FFmpeg: hlsFFmpeg(t, filepath.Join(t.TempDir(), "args"), []string{"720p", "480p", "360p"}, "")}
	rung := func(name string) video.Rendition {
		return video.Rendition{Name: name, MaxWidth: 640, MaxHeight: 360, VideoBitRate: 1}
	}
	tooMany := make([]video.Rendition, video.MaxRenditions+1)
	for i := range tooMany {
		tooMany[i] = rung(strings.Repeat("a", i+1))
	}
	for name, opts := range map[string]video.HLS{
		"negative":         {MaxOutput: -1},
		"short segment":    {SegmentDuration: time.Millisecond},
		"long segment":     {SegmentDuration: time.Hour},
		"preset":           {Preset: "placebo; rm"},
		"empty ladder":     {Renditions: []video.Rendition{}},
		"too many":         {Renditions: tooMany},
		"name with slash":  {Renditions: []video.Rendition{rung("../x")}},
		"name with space":  {Renditions: []video.Rendition{rung("a b")}},
		"name master":      {Renditions: []video.Rendition{rung("master")}},
		"upper-case name":  {Renditions: []video.Rendition{rung("HD")}},
		"duplicate":        {Renditions: []video.Rendition{rung("a"), rung("a")}},
		"no bit rate":      {Renditions: []video.Rendition{{Name: "a", MaxWidth: 1, MaxHeight: 1}}},
		"no box":           {Renditions: []video.Rendition{{Name: "a", VideoBitRate: 1}}},
		"longer than max":  {MaxDuration: time.Second},
		"var_stream_map ,": {Renditions: []video.Rendition{rung("a,name:b")}},
	} {
		want := filesystem.CodeInvalidArgument
		if name == "longer than max" {
			want = filesystem.CodeRejected
		}
		if err := tool.HLS(t.Context(), strings.NewReader("v"), opts, (&written{}).write); filesystem.CodeOf(err) != want {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := tool.HLS(t.Context(), strings.NewReader("v"), video.HLS{}, nil); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("nil writer: %v", err)
	}
	boom := errors.New("disk full")
	var calls int
	err := tool.HLS(t.Context(), strings.NewReader("v"), video.HLS{}, func(context.Context, string, string, io.Reader) error {
		calls++
		return boom
	})
	if !errors.Is(err, boom) || calls != 1 {
		t.Errorf("writer error: %v after %d calls", err, calls)
	}
}

// TestHLS_RealFFmpeg encodes the test clip for real where FFmpeg is
// installed: a two-rung ladder whose playlists name only files that were
// written, and whose segments ffprobe reads at the rung's size.
func TestHLS_RealFFmpeg(t *testing.T) {
	body := clip(t)
	var w written
	opts := video.HLS{SegmentDuration: time.Second, Preset: "ultrafast", Renditions: []video.Rendition{
		{Name: "240p", MaxWidth: 426, MaxHeight: 240, VideoBitRate: 400_000},
		{Name: "120p", MaxWidth: 160, MaxHeight: 120, VideoBitRate: 100_000},
	}}
	if err := (video.Tool{}).HLS(t.Context(), bytes.NewReader(body), opts, w.write); err != nil {
		t.Fatal(err)
	}
	if w.names[len(w.names)-1] != video.HLSMaster || !strings.Contains(w.bodies[video.HLSMaster], "120p.m3u8") || !strings.Contains(w.bodies[video.HLSMaster], "240p.m3u8") {
		t.Fatalf("master:\n%s\nnames %v", w.bodies[video.HLSMaster], w.names)
	}
	for _, pl := range []string{"240p.m3u8", "120p.m3u8"} {
		for _, line := range strings.Split(w.bodies[pl], "\n") {
			if line != "" && !strings.HasPrefix(line, "#") {
				if _, ok := w.bodies[line]; !ok {
					t.Errorf("%s names %q, which was not written", pl, line)
				}
			}
		}
	}
	info, err := video.Tool{Formats: []string{"mpegts"}}.Probe(t.Context(), strings.NewReader(w.bodies["120p_00000.ts"]))
	if err != nil || info.Width != 160 || info.Height != 120 || info.VideoCodec != "h264" {
		t.Fatalf("120p segment = %+v, %v", info, err)
	}
}

// hlsEnv is a records store whose plugin writes ladders with fake
// programs; failing makes the fake ffmpeg write a segment and fail.
func hlsEnv(t *testing.T, failing bool, opts ...video.PluginOption) (*env, *filesystem.Disk) {
	t.Helper()
	extra := ""
	if failing {
		extra = "exit 1"
	}
	tool := video.Tool{FFprobe: hlsProbe(t, false), FFmpeg: hlsFFmpeg(t, filepath.Join(t.TempDir(), "args"), []string{"720p", "480p", "360p"}, extra)}
	return twoDiskEnv(t, video.NewPlugin(tool, append([]video.PluginOption{video.WithHLS(video.HLS{})}, opts...)...))
}

// fakeMP4 sniffs as video/mp4 (an ftyp box), enough for the upload policy;
// the fake programs never read it.
var fakeMP4 = append([]byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomiso2"), make([]byte, 64)...)

// ladderKeys lists the keys under the directory of a ladder's master.
func ladderKeys(t *testing.T, d *filesystem.Disk, master string) []string {
	t.Helper()
	dir := master[:strings.LastIndex(master, "/")+1]
	var out []string
	for _, k := range keysOn(t, d) {
		if strings.HasPrefix(k, dir) {
			out = append(out, k)
		}
	}
	return out
}

// TestPlugin_HLSLadderFollowsPosterRules pins WithHLS end to end with
// fake programs: the ladder is written beside the video in a directory
// of its own and recorded in MetaHLSKey; the sweep keeps all of it while
// the row names it; a move to another disk takes every file; the purge
// deletes every file after it commits.
func TestPlugin_HLSLadderFollowsPosterRules(t *testing.T) {
	e, cdn := hlsEnv(t, false)
	f, err := e.upload(t, fakeMP4)
	if err != nil {
		t.Fatal(err)
	}
	disk, master, ok := video.HLSLocation(f)
	if !ok || disk != "media" || !strings.HasPrefix(master, f.Key+video.HLSInfix) || !strings.HasSuffix(master, "/"+video.HLSMaster) {
		t.Fatalf("ladder at %q %q (%v)", disk, master, f.Metadata)
	}
	if got := ladderKeys(t, e.disk, master); len(got) != 10 {
		t.Fatalf("ladder files = %v", got)
	}
	if n := sweepAll(t, e.store, "media"); n != 0 {
		t.Fatalf("the sweep deleted %d files of a live ladder", n)
	}

	moved, err := e.store.MoveFile(t.Context(), scope, f.ID, "cdn")
	if err != nil {
		t.Fatal(err)
	}
	if got := ladderKeys(t, cdn, master); len(got) != 10 {
		t.Fatalf("moved ladder files = %v", got)
	}
	if got := ladderKeys(t, e.disk, master); len(got) != 0 {
		t.Fatalf("the old ladder stayed: %v", got)
	}
	if n := sweepAll(t, e.store, "cdn"); n != 0 {
		t.Fatalf("the sweep deleted %d files of a moved ladder", n)
	}

	if err := e.store.Purge(t.Context(), scope, moved.ID); err != nil {
		t.Fatal(err)
	}
	if got := ladderKeys(t, cdn, master); len(got) != 0 {
		t.Fatalf("the purge left %v", got)
	}
}

// TestPlugin_HLSLocationAndReplace pins WithLocation with DerivedHLS (the
// ladder on another disk, recorded in MetaHLSDisk, the default name for a
// poster unchanged) and a replacement, which writes a new ladder and
// deletes the old one after the commit.
func TestPlugin_HLSLocationAndReplace(t *testing.T) {
	var names []string
	onCDN := func(f records.File, name string) (string, string, error) {
		names = append(names, name)
		return "cdn", "hls/" + f.ID + "-", nil
	}
	e, cdn := hlsEnv(t, false, video.WithLocation(onCDN))
	f, err := e.upload(t, fakeMP4)
	if err != nil {
		t.Fatal(err)
	}
	disk, master, ok := video.HLSLocation(f)
	if !ok || disk != "cdn" || f.Metadata[video.MetaHLSDisk] != "cdn" || !strings.HasPrefix(master, "hls/"+f.ID+"-") || !slices.Equal(names, []string{video.DerivedHLS}) {
		t.Fatalf("ladder at %q %q, names %v (%v)", disk, master, names, f.Metadata)
	}
	next, err := e.store.Replace(t.Context(), scope, f.ID, bytes.NewReader(append(fakeMP4, 1)), "clip.mp4", videoPolicy)
	if err != nil {
		t.Fatal(err)
	}
	_, nextMaster, _ := video.HLSLocation(next)
	if nextMaster == master || len(ladderKeys(t, cdn, nextMaster)) != 10 || len(ladderKeys(t, cdn, master)) != 0 {
		t.Fatalf("replace: old %q (%d left), new %q (%d)", master, len(ladderKeys(t, cdn, master)), nextMaster, len(ladderKeys(t, cdn, nextMaster)))
	}
}

// TestPlugin_HLSFailureLeavesNothing pins the cleanup: an encode that
// fails after writing (here FFmpeg exits 1, a CodeRejected video without
// WithStrict) records the file without metadata, and no file of the
// ladder stays on the disk. A Location error fails the upload closed.
func TestPlugin_HLSFailureLeavesNothing(t *testing.T) {
	e, _ := hlsEnv(t, true)
	f, err := e.upload(t, fakeMP4)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := video.HLSLocation(f); ok {
		t.Fatalf("a failed ladder was recorded: %v", f.Metadata)
	}
	for _, k := range keysOn(t, e.disk) {
		if strings.Contains(k, video.HLSInfix) {
			t.Fatalf("a file of the failed ladder stayed: %s", k)
		}
	}

	// A write failing midway (the master, written last): the segments
	// already stored are deleted at once, whether or not the upload then
	// commits.
	for name, code := range map[string]filesystem.Code{"rejected": filesystem.CodeRejected, "unavailable": filesystem.CodeUnavailable} {
		failMaster := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
			if op.Kind == filesystem.OpWrite && strings.HasSuffix(op.Key, video.HLSMaster) {
				return filesystem.Errorf(code, "refused")
			}
			return call(ctx)
		}
		media, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(failMaster))
		mgr, _ := filesystem.NewManager(media)
		db, _ := recordstest.Open(t)
		tool := video.Tool{FFprobe: hlsProbe(t, false), FFmpeg: hlsFFmpeg(t, filepath.Join(t.TempDir(), "args"), []string{"720p", "480p", "360p"}, "")}
		store, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(video.NewPlugin(tool, video.WithHLS(video.HLS{}))))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
		_, err = store.Upload(t.Context(), scope, "media", bytes.NewReader(fakeMP4), "clip.mp4", videoPolicy)
		if (code == filesystem.CodeRejected) != (err == nil) {
			t.Errorf("%s: upload err = %v", name, err)
		}
		for _, k := range keysOn(t, media) {
			if strings.Contains(k, video.HLSInfix) {
				t.Errorf("%s: a file of the failed ladder stayed: %s", name, k)
			}
		}
	}

	refuse := errors.New("no room for ladders")
	e, _ = hlsEnv(t, false, video.WithLocation(func(records.File, string) (string, string, error) { return "", "", refuse }))
	if _, err := e.upload(t, fakeMP4); !errors.Is(err, refuse) {
		t.Fatalf("a Location error: %v", err)
	}
}

// TestHLS_ProbeFailure: a body ffprobe cannot read fails HLS before any
// encode, and nothing reaches the writer.
func TestHLS_ProbeFailure(t *testing.T) {
	log := filepath.Join(t.TempDir(), "args")
	tool := video.Tool{FFprobe: fakeProgram(t, "ffprobe", "echo 'not a video' >&2; exit 1"), FFmpeg: hlsFFmpeg(t, log, []string{"360p"}, "")}
	var w written
	if err := tool.HLS(t.Context(), strings.NewReader("text"), video.HLS{}, w.write); err == nil {
		t.Fatal("HLS of an unreadable body succeeded")
	}
	if len(w.names) != 0 {
		t.Errorf("the writer received %v", w.names)
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ffmpeg ran after a failed probe: %v", err)
	}
}

// hlsFaultEnv is hlsEnv with a switch that fails every listing of the
// media disk, and the plugin's HLS options.
func hlsFaultEnv(t *testing.T, opts video.HLS, popts ...video.PluginOption) (*env, *atomic.Bool) {
	t.Helper()
	var listDown atomic.Bool
	failList := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpList && listDown.Load() {
			return filesystem.Errorf(filesystem.CodeUnavailable, "listing down")
		}
		return call(ctx)
	}
	media, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(failList))
	cdn, _ := filesystem.NewDisk("cdn", memory.New())
	mgr, _ := filesystem.NewManager(media, cdn)
	db, _ := recordstest.Open(t)
	tool := video.Tool{FFprobe: hlsProbe(t, false), FFmpeg: hlsFFmpeg(t, filepath.Join(t.TempDir(), "args"), []string{"720p", "480p", "360p"}, "")}
	store, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(video.NewPlugin(tool, append([]video.PluginOption{video.WithHLS(opts)}, popts...)...)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &env{disk: media, store: store}, &listDown
}

// TestPlugin_HLSRefusals: a video longer than the ladder's MaxDuration
// gets no ladder (CodeRejected, recorded without one), and a Location
// naming a disk the Store does not have fails the upload closed.
func TestPlugin_HLSRefusals(t *testing.T) {
	e, _ := hlsFaultEnv(t, video.HLS{MaxDuration: time.Second}) // the fake probe reports 4 seconds
	f, err := e.upload(t, fakeMP4)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := video.HLSLocation(f); ok {
		t.Fatalf("a video over MaxDuration got a ladder: %v", f.Metadata)
	}
	nowhere := func(f records.File, _ string) (string, string, error) { return "nowhere", "hls/" + f.ID + "-", nil }
	e, _ = hlsFaultEnv(t, video.HLS{}, video.WithLocation(nowhere))
	if _, err := e.upload(t, fakeMP4); err == nil {
		t.Fatal("a ladder for an unknown disk was accepted")
	}
}

// TestPlugin_HLSMoveFailures: a move whose listing of the ladder fails,
// or whose ladder lost its master, fails without moving the file; a purge
// whose listing after the commit fails still purges, leaving the ladder
// to SweepOrphans.
func TestPlugin_HLSMoveFailures(t *testing.T) {
	ctx := t.Context()
	e, listDown := hlsFaultEnv(t, video.HLS{})
	f, err := e.upload(t, fakeMP4)
	if err != nil {
		t.Fatal(err)
	}
	listDown.Store(true)
	if _, err := e.store.MoveFile(ctx, scope, f.ID, "cdn"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("move with the listing down = %v", err)
	}
	listDown.Store(false)
	_, master, _ := video.HLSLocation(f)
	if err := e.disk.Delete(ctx, master); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.MoveFile(ctx, scope, f.ID, "cdn"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("move of a ladder without its master = %v", err)
	}
	if got, err := e.store.File(ctx, scope, f.ID); err != nil || got.Disk != "media" {
		t.Fatalf("a failed move changed the file: %+v, %v", got, err)
	}
	listDown.Store(true)
	if err := e.store.Purge(ctx, scope, f.ID); err != nil {
		t.Fatalf("purge with the listing down = %v", err)
	}
	listDown.Store(false)
	if len(ladderKeys(t, e.disk, master)) == 0 {
		t.Fatal("the ladder went although its listing failed; the test proves nothing")
	}
}

// TestPlugin_HLSPosterFailure: with a poster and a ladder, a poster FFmpeg
// cannot make records the file without either, as a failed poster alone
// does (no WithStrict).
func TestPlugin_HLSPosterFailure(t *testing.T) {
	tool := video.Tool{FFprobe: hlsProbe(t, false), FFmpeg: fakeProgram(t, "ffmpeg", "exit 1")}
	e, _ := twoDiskEnv(t, video.NewPlugin(tool, video.WithPoster(time.Second), video.WithHLS(video.HLS{})))
	f, err := e.upload(t, fakeMP4)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := video.HLSLocation(f); ok || f.Metadata[video.MetaPosterKey] != "" {
		t.Fatalf("a failed poster still recorded derived files: %v", f.Metadata)
	}
}
