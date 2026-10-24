package video

import (
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video/ffmpeg"
	"github.com/ubgo/filesystem/upload"
)

// typePrefix marks the content types Inspect and Sanitize act on.
const typePrefix = "video/"

// IsVideo reports whether contentType is a video type ("video/mp4").
func IsVideo(contentType string) bool { return strings.HasPrefix(contentType, typePrefix) }

// Limits bound what Inspect accepts. Zero fields do not limit.
type Limits struct {
	// MaxDuration refuses longer videos, by the container's claim and by
	// the packets' timestamps (ffmpeg.Runner.CheckLength, one more
	// ffprobe pass over the file), because the claim is a header a client
	// can patch; a video whose packets carry no timestamps is refused too.
	MaxDuration time.Duration
	// MaxWidth and MaxHeight refuse larger display sizes (4K is 3840×2160).
	MaxWidth, MaxHeight int
}

// Inspect returns an upload.Inspector that probes every stored file whose
// type is a video type and refuses it (CodeRejected, which deletes the
// object) when ffprobe cannot read it as a video or it is over limits.
// Other types pass untouched, so it composes with other inspectors
// (upload.Inspectors). Use it as Policy.Inspect: it reads the stored
// object back, so it covers presigned and Uppy uploads through Confirm
// as well as form uploads.
func (t Tool) Inspect(limits Limits) upload.Inspector {
	return func(ctx context.Context, disk *filesystem.Disk, r upload.Result) error {
		if !IsVideo(r.ContentType) {
			return nil
		}
		rc, err := disk.Read(ctx, r.Key)
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		return t.withWorkFile(rc, func(path string) error {
			info, err := t.probePath(ctx, path)
			if err != nil {
				return err
			}
			if err := t.runner().CheckLength(ctx, t.program(t.FFprobe, DefaultFFprobe), path, info.Duration, limits.MaxDuration); err != nil {
				return err
			}
			return limits.checkSize(info)
		})
	}
}

// checkSize refuses info when its display size is over the limits.
func (l Limits) checkSize(info Info) error {
	if l.MaxWidth > 0 && info.Width > l.MaxWidth || l.MaxHeight > 0 && info.Height > l.MaxHeight {
		return filesystem.Errorf(filesystem.CodeRejected, "video is larger than %dx%d", l.MaxWidth, l.MaxHeight)
	}
	return nil
}

// muxers maps the video types Sanitize rewrites to FFmpeg's muxer for
// them; a video type not listed passes through unchanged.
var muxers = map[string]string{
	"video/mp4":        "mp4",
	"video/quicktime":  "mov",
	"video/webm":       "webm",
	"video/x-matroska": "matroska",
}

// Sanitize returns an upload.Processor that removes the metadata of every
// video it can rewrite (MP4, QuickTime, WebM, Matroska): container and
// stream tags such as a phone's recording location and device, chapters,
// and every stream but the video and audio (phones store the location in
// a timed metadata track too). It remuxes without re-encoding, so it is
// fast and lossless. Other types, and video types it has no muxer for,
// pass through unchanged. Use it as Policy.Process.
//
// The rewritten video is read from a work file that is unlinked as soon
// as it is opened, so nothing is left on disk whatever happens to the
// upload.
func (t Tool) Sanitize() upload.Processor {
	return func(ctx context.Context, in upload.Info, body io.Reader) (io.Reader, upload.Info, error) {
		muxer, ok := muxers[mediaType(in.ContentType)]
		if !ok {
			return body, in, nil
		}
		var out *os.File
		err := t.withWorkFile(body, func(path string) error {
			var err error
			out, err = t.remux(ctx, path, muxer)
			return err
		})
		if err != nil {
			return nil, upload.Info{}, err
		}
		return ffmpeg.CloseAtEOF(out), in, nil
	}
}

// remuxGrowth bounds a remux's output against the input cap: copying
// packets into another container changes the size by its overhead only,
// so twice the cap is room for any real file and still stops a crafted
// one from filling the disk.
const remuxGrowth = 2

// remux rewrites path without metadata into a work file (Runner.
// OutputFile, so FFmpeg stops at the cap and the file is unlinked once
// open).
func (t Tool) remux(ctx context.Context, path, muxer string) (*os.File, error) {
	run := t.runner()
	limit := remuxGrowth * t.maxInput()
	return run.OutputFile(limit, func(dst string) error {
		args := append([]string{"-v", "error", "-nostdin"}, run.InputOptions()...)
		args = append(args, "-i", path,
			"-map", "0:v", "-map", "0:a?", "-c", "copy",
			"-map_metadata", "-1", "-map_metadata:s", "-1", "-map_chapters", "-1",
			"-fflags", "+bitexact", "-flags:v", "+bitexact", "-flags:a", "+bitexact")
		args = append(args, ffmpeg.OutputLimit(limit)...)
		args = append(args, "-f", muxer, "-y", dst)
		_, err := run.Run(ctx, t.program(t.FFmpeg, DefaultFFmpeg), args, ffmpeg.MaxFileRunOutput)
		return err
	})
}

// mediaType strips parameters from a content type.
func mediaType(contentType string) string {
	mt, _, _ := strings.Cut(contentType, ";")
	return strings.TrimSpace(strings.ToLower(mt))
}
