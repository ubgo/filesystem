package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video"
	"github.com/ubgo/filesystem/records"
)

// Decisions about the derived files, not the library's.
const (
	// webMaxSide bounds the web copy: 720p is plenty for a feed.
	webMaxSide = 1280
	// storyboardEvery is the time between two storyboard tiles; short,
	// because the demo clip is three seconds long.
	storyboardEvery = 500 * time.Millisecond
)

// webMP4 re-encodes a stored video as a web MP4 (H.264 and AAC, the index
// first, rotation applied, metadata dropped) no larger than 1280x720, and
// writes it next to the original. Encoding is slow: run it from a job
// for anything longer than a chat clip.
// ds:def id=webmp4-h4c9w2tn
func webMP4(ctx context.Context, disk *filesystem.Disk, f records.File) (string, error) {
	src, err := disk.Read(ctx, f.Key)
	if err != nil {
		return "", err
	}
	defer func() { _ = src.Close() }()
	out, err := tool.Transcode(ctx, src, video.Transcode{MaxWidth: webMaxSide, MaxHeight: webMaxSide * 9 / 16})
	if err != nil {
		return "", err
	}
	defer func() { _ = out.Close() }()
	key := f.Key + ".web.mp4"
	_, err = disk.Write(ctx, key, out, filesystem.WriteOptions{ContentType: video.MP4ContentType})
	return key, err
}

// storyboard makes the sprite a player shows above its timeline while
// scrubbing: one JPEG holding a grid of frames, with the time span of
// each tile, which the app turns into a WebVTT thumbnails track.
// ds:def id=storyboard-p6t3m8xk
func storyboard(ctx context.Context, disk *filesystem.Disk, f records.File) (video.Sprite, error) {
	src, err := disk.Read(ctx, f.Key)
	if err != nil {
		return video.Sprite{}, err
	}
	defer func() { _ = src.Close() }()
	return tool.Storyboard(ctx, src, video.Storyboard{Interval: storyboardEvery, Columns: 3, TileSize: 120})
}

// derived prints the web copy and the storyboard of f.
func derived(ctx context.Context, out io.Writer, disk *filesystem.Disk, f records.File) error {
	key, err := webMP4(ctx, disk, f)
	if err != nil {
		return err
	}
	web, err := disk.Stat(ctx, key)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "web copy  %s (%s)\n", key[len(key)-len(".web.mp4"):], web.ContentType)
	sprite, err := storyboard(ctx, disk, f)
	if err != nil {
		return err
	}
	last := sprite.Tiles[len(sprite.Tiles)-1]
	fmt.Fprintf(out, "storyboard %d tiles of %dx%d, %d per row, last %s-%s at %d,%d\n",
		len(sprite.Tiles), sprite.TileWidth, sprite.TileHeight, sprite.Columns, last.Start, last.End, last.X, last.Y)
	return hls(ctx, out, disk, f)
}
