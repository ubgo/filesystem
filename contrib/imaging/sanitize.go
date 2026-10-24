package imaging

import (
	"bytes"
	"context"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/upload"
)

// SanitizeQuality is the JPEG quality Sanitize re-encodes at unless
// WithQuality says otherwise: high, since the original is not kept.
const SanitizeQuality = 90

// SanitizeOption configures Sanitize.
type SanitizeOption func(*sanitizeConfig)

type sanitizeConfig struct {
	quality  int
	pipeline *Pipeline
}

// WithPipeline decodes with p instead of the default Pipeline, so formats
// its codec modules read (HEIC, AVIF) are sanitized too.
func WithPipeline(p *Pipeline) SanitizeOption {
	return func(c *sanitizeConfig) { c.pipeline = p }
}

// WithQuality sets the JPEG quality Sanitize re-encodes at, 1 to 100, in
// place of SanitizeQuality. A photo already saved at a lower quality grows
// when re-encoded at 90; a lower setting keeps such uploads from growing,
// at the cost of detail (every re-encode loses some, whatever the
// setting). A value outside 1..100 is a programming error: the processor
// then refuses every JPEG with CodeInvalidArgument rather than guess.
func WithQuality(q int) SanitizeOption {
	return func(c *sanitizeConfig) { c.quality = q }
}

// sanitizedTypes are the types Sanitize re-encodes. GIF passes through:
// re-encoding keeps only its first frame, and it carries no EXIF.
var sanitizedTypes = []string{"image/jpeg", "image/png", "image/webp", "image/bmp", "image/tiff", "image/heic", "image/heif", "image/avif"}

// photoTypes become JPEG: they are lossy camera formats, and as PNG a
// photo would grow several times. The others become lossless PNG.
var photoTypes = []string{"image/jpeg", "image/heic", "image/heif", "image/avif"}

// Sanitize returns an upload.Processor (for upload.Policy.Process) that
// re-encodes photos before they are stored: the EXIF orientation is
// applied, and everything else the file carried is dropped, including the
// GPS position phones record, camera serial numbers, XMP, comments and any
// payload appended after the image. Dimensions are kept. JPEG stays JPEG
// (at SanitizeQuality, or WithQuality); PNG, WebP, BMP and TIFF become PNG, the lossless
// format the standard library can write, and the file name's extension
// follows. Other files pass through untouched. An image that does not
// decode, or is over the limits of opts, is refused (CodeRejected or
// CodeTooLarge).
func Sanitize(opts Options, options ...SanitizeOption) upload.Processor {
	o := opts.withDefaults()
	c := sanitizeConfig{quality: SanitizeQuality, pipeline: defaultPipeline}
	for _, opt := range options {
		opt(&c)
	}
	return func(_ context.Context, in upload.Info, body io.Reader) (io.Reader, upload.Info, error) {
		if !slices.Contains(sanitizedTypes, in.ContentType) {
			return body, in, nil
		}
		photo := slices.Contains(photoTypes, in.ContentType)
		if photo && (c.quality < 1 || c.quality > 100) {
			return nil, upload.Info{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: sanitize quality %d is not within 1..100", c.quality)
		}
		data, err := readLimited(body, o.MaxBytes)
		if err != nil {
			return nil, upload.Info{}, err
		}
		img, err := c.pipeline.decode(data, o.MaxPixels)
		if err != nil {
			return nil, upload.Info{}, err
		}
		img, profile, err := c.pipeline.manageColour(data, img)
		if err != nil {
			return nil, upload.Info{}, err
		}
		img = orient(img, exifOrientation(data))
		// Encoding into a bytes.Buffer cannot fail for a decoded image
		// within MaxPixels (see Process).
		var buf bytes.Buffer
		out := upload.Info{Filename: in.Filename, ContentType: "image/jpeg"}
		ext, format := ".jpg", FormatJPEG
		if photo {
			_ = jpegEncoder{}.Encode(&buf, img, c.quality)
		} else {
			_ = pngEncoder{}.Encode(&buf, img, 0)
			out.ContentType, ext, format = "image/png", ".png", FormatPNG
		}
		if in.ContentType != out.ContentType {
			out.Filename = strings.TrimSuffix(in.Filename, path.Ext(in.Filename)) + ext
		}
		if profile != nil {
			return bytes.NewReader(embedProfile(format, buf.Bytes(), profile)), out, nil
		}
		return &buf, out, nil
	}
}
