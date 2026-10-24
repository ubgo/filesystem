package audio

import (
	"context"
	"io"
	"path"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video/ffmpeg"
	"github.com/ubgo/filesystem/upload"
)

// typePrefix marks the content types Inspect, TranscodeProcessor and the
// plugin act on.
const typePrefix = "audio/"

// IsAudio reports whether contentType is an audio type ("audio/ogg").
func IsAudio(contentType string) bool { return strings.HasPrefix(contentType, typePrefix) }

// Limits bound what Inspect accepts. Zero fields do not limit.
type Limits struct {
	// MaxDuration refuses longer audio, by the container's claim and by
	// the packets' timestamps (ffmpeg.Runner.CheckLength, one more
	// ffprobe pass over the file), because the claim is a header a client
	// can patch; audio whose packets carry no timestamps is refused too.
	MaxDuration time.Duration
}

// Inspect returns an upload.Inspector that probes every stored file whose
// type is an audio type and refuses it (CodeRejected, which deletes the
// object) when ffprobe cannot read it as audio or it is over limits.
// Other types pass untouched, so it composes with other inspectors. It
// reads the stored object back, so it covers presigned uploads through
// Confirm as well as form uploads.
func (t Tool) Inspect(limits Limits) upload.Inspector {
	return func(ctx context.Context, disk *filesystem.Disk, r upload.Result) error {
		if !IsAudio(r.ContentType) {
			return nil
		}
		rc, err := disk.Read(ctx, r.Key)
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		run := t.runner()
		return run.WorkFile(rc, func(path string) error {
			info, err := t.probePath(ctx, path)
			if err != nil {
				return err
			}
			return run.CheckLength(ctx, t.ffprobe(), path, info.Duration, limits.MaxDuration)
		})
	}
}

// TranscodeProcessor returns an upload.Processor that transcodes every
// audio upload (Transcode) and gives it the format's type and extension;
// other types pass untouched. It runs while the client waits, which suits
// voice notes and short clips; a long recording belongs in a job.
func (t Tool) TranscodeProcessor(opts Transcode) upload.Processor {
	return func(ctx context.Context, in upload.Info, body io.Reader) (io.Reader, upload.Info, error) {
		if !IsAudio(in.ContentType) {
			return body, in, nil
		}
		f, err := opts.format()
		if err != nil {
			return nil, upload.Info{}, err
		}
		rc, err := t.Transcode(ctx, body, opts)
		if err != nil {
			return nil, upload.Info{}, err
		}
		in.ContentType = f.contentType
		in.Filename = withExtension(in.Filename, f.extension)
		if in.Path != "" {
			in.Path = withExtension(in.Path, f.extension)
		}
		return ffmpeg.CloseAtEOF(rc), in, nil
	}
}

// withExtension replaces name's extension with ext.
func withExtension(name, ext string) string {
	return strings.TrimSuffix(name, path.Ext(name)) + ext
}
