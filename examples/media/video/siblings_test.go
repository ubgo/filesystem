package main

import (
	"bytes"
	"database/sql"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ubgo/sqlb"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging/dimensions"
	"github.com/ubgo/filesystem/contrib/video"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
)

// siblingClip is an ffmpeg-made 320x240 video, the size video records.
func siblingClip(t *testing.T) []byte {
	t.Helper()
	for _, prog := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(prog); err != nil {
			t.Skipf("%s is not installed", prog)
		}
	}
	path := filepath.Join(t.TempDir(), "clip.mp4")
	out, err := exec.CommandContext(t.Context(), "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=10:duration=1",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// siblingPNG is a blank w by h PNG.
func siblingPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// siblingDims is the dimensions plugin's reading of f: width, height and
// 1 when it recorded them.
func siblingDims(f records.File) [3]int {
	w, h, ok := dimensions.Of(f)
	if !ok {
		return [3]int{w, h, 0}
	}
	return [3]int{w, h, 1}
}

// siblingEnv is a Store with the dimensions and video plugins, installed
// in the order given: hooks run in install order, and the plugins share
// the width and height keys.
func siblingEnv(t *testing.T, videoFirst bool) (*filesystem.Disk, *records.Store) {
	t.Helper()
	disk, err := filesystem.NewDisk(diskMedia, memory.New())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := filesystem.NewManager(disk)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // one connection keeps the in-memory database alive
	t.Cleanup(func() { _ = db.Close() })
	plugins := []records.Plugin{dimensions.New(), video.NewPlugin(tool)}
	if videoFirst {
		plugins[0], plugins[1] = plugins[1], plugins[0]
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, mgr, records.WithPlugins(plugins...))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return disk, s
}

// TestSiblingPluginsKeepEachOthersKeys pins that the dimensions and video
// plugins, which share the width and height keys, never clear them on a
// file of the other's type, in either install order, on create and on a
// replacement across the two types.
func TestSiblingPluginsKeepEachOthersKeys(t *testing.T) {
	clip := siblingClip(t)
	for _, videoFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "dimensions first", true: "video first"}[videoFirst], func(t *testing.T) {
			disk, s := siblingEnv(t, videoFirst)
			ctx := t.Context()
			put := func(key string, body []byte) {
				if _, err := disk.WriteBytes(ctx, key, body, filesystem.WriteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			put("a.png", siblingPNG(t, 80, 30))
			put("a.mp4", clip)
			img, err := s.CreateFile(ctx, records.NewFile{Scope: scope, Disk: diskMedia, Key: "a.png", ContentType: "image/png"})
			if err != nil {
				t.Fatal(err)
			}
			if siblingDims(img) != [3]int{80, 30, 1} {
				t.Fatalf("image dims = %v (%v)", siblingDims(img), img.Metadata)
			}
			vid, err := s.CreateFile(ctx, records.NewFile{Scope: scope, Disk: diskMedia, Key: "a.mp4", ContentType: "video/mp4"})
			if err != nil {
				t.Fatal(err)
			}
			if m, ok := video.Of(vid); !ok || m.Width != 320 || m.Height != 240 {
				t.Fatalf("video meta = %+v %v (%v)", m, ok, vid.Metadata)
			}
			// The image's content becomes a video: video's values win.
			put("b.mp4", clip)
			next, err := s.ReplaceObject(ctx, scope, img.ID, records.NewContent{Key: "b.mp4", ContentType: "video/mp4"})
			if err != nil {
				t.Fatal(err)
			}
			if m, ok := video.Of(next); !ok || m.Width != 320 || m.Height != 240 {
				t.Fatalf("image replaced by video: meta = %+v %v (%v)", m, ok, next.Metadata)
			}
			// The video's content becomes an image: dimensions' values win,
			// and no video-only key outlives the video.
			put("b.png", siblingPNG(t, 64, 48))
			back, err := s.ReplaceObject(ctx, scope, vid.ID, records.NewContent{Key: "b.png", ContentType: "image/png"})
			if err != nil {
				t.Fatal(err)
			}
			if siblingDims(back) != [3]int{64, 48, 1} {
				t.Fatalf("video replaced by image: dims = %v (%v)", siblingDims(back), back.Metadata)
			}
			if _, ok := video.Of(back); ok || back.Metadata[video.MetaVideoCodec] != "" || back.Metadata[video.MetaDuration] != "" {
				t.Fatalf("video keys outlived the video: %v", back.Metadata)
			}
		})
	}
}
