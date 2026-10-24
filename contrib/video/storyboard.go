package video

import (
	"context"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video/ffmpeg"
)

// Defaults of Storyboard, used when a field is zero.
const (
	// DefaultStoryboardInterval is the time between two tiles.
	DefaultStoryboardInterval = 10 * time.Second
	// DefaultStoryboardColumns is the tiles per row of the sprite.
	DefaultStoryboardColumns = 10
	// DefaultTileSize is the longest side of a tile, in pixels.
	DefaultTileSize = 160
	// DefaultMaxTiles bounds the tiles of one sprite: a longer video gets
	// a wider interval rather than a larger image.
	DefaultMaxTiles = 100
)

// Upper bounds of Storyboard's fields, so a sprite's pixel count stays
// bounded whatever the caller passes: at most MaxTiles × MaxTileSize²
// pixels.
const (
	MaxTileSize = 512
	MaxTiles    = 400
)

// Storyboard configures Tool.Storyboard. The zero value uses the
// defaults.
type Storyboard struct {
	// Interval between tiles; 0 means DefaultStoryboardInterval. It
	// widens when the video would need more than MaxTiles tiles.
	Interval time.Duration
	// Columns of tiles per row; 0 means DefaultStoryboardColumns.
	Columns int
	// TileSize is a tile's longest side in pixels, 1 to MaxTileSize; 0
	// means DefaultTileSize. The other side follows the video's aspect
	// ratio.
	TileSize int
	// MaxTiles bounds the tiles, 1 to the package's MaxTiles; 0 means
	// DefaultMaxTiles.
	MaxTiles int
}

// Sprite is a storyboard: one JPEG holding a grid of frames taken along
// the video, what a player shows above its timeline while scrubbing.
type Sprite struct {
	// JPEG is the sprite image.
	JPEG []byte
	// TileWidth and TileHeight are each tile's size, Columns the tiles per
	// row; the image is Columns tiles wide.
	TileWidth, TileHeight, Columns int
	// Tiles in time order, each with the span it stands for and its
	// top-left corner in the image.
	Tiles []Tile
}

// Tile is one frame of a Sprite.
type Tile struct {
	Start, End time.Duration
	X, Y       int
}

// Storyboard returns a sprite of frames taken every Interval along r.
// The video's length bounds the work: at most MaxTiles frames, each
// decoded once, in one ffmpeg run under the Tool's Timeout, the JPEG
// capped by the Tool's MaxPoster. A file that is not a readable video is
// CodeRejected; options out of range are CodeInvalidArgument.
func (t Tool) Storyboard(ctx context.Context, r io.Reader, opts Storyboard) (Sprite, error) {
	if err := opts.check(); err != nil {
		return Sprite{}, err
	}
	var sprite Sprite
	err := t.withWorkFile(r, func(path string) error {
		info, err := t.probePath(ctx, path)
		if err != nil {
			return err
		}
		if info.Duration <= 0 || info.Width <= 0 || info.Height <= 0 {
			return filesystem.Errorf(filesystem.CodeRejected, "video: no duration or size to make a storyboard from")
		}
		var interval time.Duration
		sprite, interval = opts.layout(info)
		jpg, err := t.run(ctx, t.program(t.FFmpeg, DefaultFFmpeg), sprite.args(t.inputOptions(), path, interval, info.Duration), t.maxPoster())
		if err != nil {
			return err
		}
		if len(jpg) == 0 {
			return filesystem.Errorf(filesystem.CodeRejected, "video: no frames to make a storyboard from")
		}
		sprite.JPEG = jpg
		return nil
	})
	if err != nil {
		return Sprite{}, err
	}
	return sprite, nil
}

// check refuses options out of range.
func (o Storyboard) check() error {
	if o.Interval < 0 || o.Columns < 0 || o.TileSize < 0 || o.TileSize > MaxTileSize || o.MaxTiles < 0 || o.MaxTiles > MaxTiles {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: storyboard options out of range (tile size 0..%d, tiles 0..%d)", MaxTileSize, MaxTiles)
	}
	return nil
}

// layout plans the sprite for a video: the tile size from the display
// aspect ratio and every tile's span and position, and returns the
// interval between tiles.
func (o Storyboard) layout(info Info) (Sprite, time.Duration) {
	interval := o.Interval
	if interval == 0 {
		interval = DefaultStoryboardInterval
	}
	maxTiles := orDefault(o.MaxTiles, DefaultMaxTiles)
	n := int(math.Ceil(float64(info.Duration) / float64(interval)))
	if n > maxTiles {
		interval = time.Duration(math.Ceil(float64(info.Duration) / float64(maxTiles)))
		n = int(math.Ceil(float64(info.Duration) / float64(interval)))
	}
	n = max(n, 1)
	size := orDefault(o.TileSize, DefaultTileSize)
	w, h := fit(info.Width, info.Height, size)
	s := Sprite{TileWidth: w, TileHeight: h, Columns: min(orDefault(o.Columns, DefaultStoryboardColumns), n)}
	for i := range n {
		// The last tile ends at the duration, which n×interval reaches or
		// passes; computing that product could wrap for the longest
		// Duration (a container claiming an absurd length).
		end := info.Duration
		if i+1 < n {
			end = time.Duration(i+1) * interval
		}
		s.Tiles = append(s.Tiles, Tile{
			Start: time.Duration(i) * interval,
			End:   end,
			X:     (i % s.Columns) * w,
			Y:     (i / s.Columns) * h,
		})
	}
	return s, interval
}

// fit scales w×h so its longest side is size, keeping the aspect ratio,
// with even sides of at least 2 pixels (what the JPEG encoder and the
// tile filter handle without padding).
func fit(w, h, size int) (int, int) {
	even := func(v float64) int { return max(2, int(math.Round(v/2))*2) }
	if w >= h {
		return even(float64(size)), even(float64(size) * float64(h) / float64(w))
	}
	return even(float64(size) * float64(w) / float64(h)), even(float64(size))
}

// args are ffmpeg's arguments to render s from path: one frame per tile
// span, scaled, tiled into one image.
func (s Sprite) args(inputOptions []string, path string, interval, duration time.Duration) []string {
	rows := (len(s.Tiles) + s.Columns - 1) / s.Columns
	// select keeps the first frame and then one per interval; the fps
	// filter would emit nothing for a video shorter than one interval.
	vf := fmt.Sprintf(`select='isnan(prev_selected_t)+gte(t-prev_selected_t\,%s)',scale=%d:%d,tile=%dx%d,format=yuvj420p`, ffmpeg.Seconds(interval), s.TileWidth, s.TileHeight, s.Columns, rows)
	args := append([]string{"-v", "error", "-nostdin"}, inputOptions...)
	return append(args, "-i", path, "-t", ffmpeg.Seconds(duration), "-map", "0:v:0", "-vf", vf,
		"-frames:v", "1", "-f", "image2", "-c:v", "mjpeg", "-q:v", "4", "pipe:1")
}

// WebVTT returns the storyboard as a WebVTT thumbnails track, the format
// players (Video.js, Plyr, Vidstack, JW Player) read for scrubbing
// previews: one cue per tile pointing at imageURL#xywh=x,y,w,h. imageURL
// is where the app serves JPEG; one holding a line break is
// CodeInvalidArgument, since it would end the cue.
func (s Sprite) WebVTT(imageURL string) (string, error) {
	if strings.ContainsAny(imageURL, "\r\n") {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "video: a storyboard URL cannot hold a line break")
	}
	var b strings.Builder
	b.WriteString("WEBVTT\n")
	for _, tile := range s.Tiles {
		fmt.Fprintf(&b, "\n%s --> %s\n%s#xywh=%d,%d,%d,%d\n", vttTime(tile.Start), vttTime(tile.End), imageURL, tile.X, tile.Y, s.TileWidth, s.TileHeight)
	}
	return b.String(), nil
}

// vttTime formats d as WebVTT's hh:mm:ss.ttt.
func vttTime(d time.Duration) string {
	ms := d.Milliseconds()
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3_600_000, ms/60_000%60, ms/1000%60, ms%1000)
}
