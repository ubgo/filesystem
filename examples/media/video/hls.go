package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video"
	"github.com/ubgo/filesystem/records"
)

// ladder is the app's HLS ladder for short clips: three rungs, small bit
// rates. A rung larger than the upload is dropped, so a 360p clip never
// gets an upscaled 720p.
var ladder = []video.Rendition{
	{Name: "720p", MaxWidth: 1280, MaxHeight: 720, VideoBitRate: 2_800_000},
	{Name: "360p", MaxWidth: 640, MaxHeight: 360, VideoBitRate: 800_000},
	{Name: "180p", MaxWidth: 320, MaxHeight: 180, VideoBitRate: 300_000},
}

// hlsDir is where a file's ladder lands: one directory per file, so the
// master's relative paths resolve to its own playlists and segments.
func hlsDir(f records.File) string { return "streams/" + f.ID + "/" }

// hlsLadder encodes a stored video as an HLS ladder on the same disk and
// returns the master playlist's key, the one URL a player is given. Each
// file goes through the writer as FFmpeg finishes the ladder: segments
// first, then the rendition playlists, then the master. Run it from a
// job for anything longer than a chat clip.
// ds:def id=hlsladder-b8r4w6ny
func hlsLadder(ctx context.Context, disk *filesystem.Disk, f records.File, written *[]string) (string, error) {
	src, err := disk.Read(ctx, f.Key)
	if err != nil {
		return "", err
	}
	defer func() { _ = src.Close() }()
	dir := hlsDir(f)
	err = tool.HLS(ctx, src, video.HLS{Renditions: ladder}, func(ctx context.Context, name, contentType string, body io.Reader) error {
		*written = append(*written, name)
		_, err := disk.Write(ctx, dir+name, body, filesystem.WriteOptions{ContentType: contentType})
		return err
	})
	return dir + video.HLSMaster, err
}

// hls prints the ladder of f: the renditions the master names, the order
// the files arrived in, and the master's type.
func hls(ctx context.Context, out io.Writer, disk *filesystem.Disk, f records.File) error {
	var written []string
	master, err := hlsLadder(ctx, disk, f, &written)
	if err != nil {
		return err
	}
	e, err := disk.Stat(ctx, master)
	if err != nil {
		return err
	}
	body, err := disk.Read(ctx, master)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	var playlists []string
	for sc := bufio.NewScanner(body); sc.Scan(); {
		if line := sc.Text(); strings.HasSuffix(line, ".m3u8") {
			playlists = append(playlists, line)
		}
	}
	isPlaylist := func(n string) bool { return strings.HasSuffix(n, ".m3u8") }
	first := slices.IndexFunc(written, isPlaylist)
	ordered := first > 0 && !slices.ContainsFunc(written[:first], isPlaylist) &&
		!slices.ContainsFunc(written[first:], func(n string) bool { return !isPlaylist(n) }) && written[len(written)-1] == video.HLSMaster
	fmt.Fprintf(out, "hls       %s (%s) names %s\n", master[len(hlsDir(f)):], e.ContentType, strings.Join(playlists, ", "))
	fmt.Fprintf(out, "hls       %d segments, then %d playlists, the master last: %t\n", first, len(written)-first, ordered)
	return nil
}
