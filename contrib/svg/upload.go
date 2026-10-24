package svg

import (
	"bytes"
	"context"
	"io"
	"slices"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/sniff"
	"github.com/ubgo/filesystem/upload"
)

// IsSVG reports whether contentType is SVG ("image/svg+xml", any
// parameters).
func IsSVG(contentType string) bool { return sniff.Matches(contentType, ContentType) }

// Processor returns an upload.Processor that sanitises every SVG upload
// with Sanitize before it is stored; other types pass untouched. A file
// that is not a well-formed SVG document is refused (CodeRejected). The
// sanitised document is held in memory, bounded by opts.MaxBytes.
func Processor(opts Options) upload.Processor {
	return func(_ context.Context, in upload.Info, body io.Reader) (io.Reader, upload.Info, error) {
		if !IsSVG(in.ContentType) {
			return body, in, nil
		}
		var out bytes.Buffer
		if _, err := Sanitize(body, &out, opts); err != nil {
			return nil, upload.Info{}, err
		}
		return &out, in, nil
	}
}

// Inspect returns an upload.Inspector that refuses a stored SVG which
// Sanitize would change (CodeRejected, which deletes the object): the
// check for uploads that skip Policy.Process (presigned and Uppy uploads,
// confirmed with upload.Confirm), so an SVG reaches storage clean or not
// at all. Other types pass untouched.
func Inspect(opts Options) upload.Inspector {
	return func(ctx context.Context, disk *filesystem.Disk, r upload.Result) error {
		if !IsSVG(r.ContentType) {
			return nil
		}
		rc, err := disk.Read(ctx, r.Key)
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		rep, err := Sanitize(rc, io.Discard, opts)
		if err != nil {
			return err
		}
		if !rep.Clean() {
			return filesystem.Errorf(filesystem.CodeRejected, "svg: the image contains active content (%s)", rep.Removed[0])
		}
		return nil
	}
}

// Policy returns base changed to accept SVG safely, the one way to let
// SVG through an upload policy that this package recommends:
//
//   - SVG is added to AllowActive, the upload package's narrow exception:
//     SVG alone is let through, and every other active type (HTML, XHTML,
//     every other +xml type, JavaScript) and the file-name rules
//     (upload.DefaultDeniedExtensions) stay refused.
//   - Process runs base.Process, then sanitises SVG (Processor); Inspect
//     refuses an SVG that is not clean (Inspect), then runs base.Inspect.
//
// base.Allowed must already accept image/svg+xml; Policy does not widen
// it. The result is validated like any policy.
func Policy(base upload.Policy, opts Options) (upload.Policy, error) {
	if !sniff.MatchesAny(ContentType, base.Allowed) {
		return upload.Policy{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "svg: the policy does not allow %s", ContentType)
	}
	p := base
	p.AllowActive = append(slices.Clone(base.AllowActive), ContentType)
	sanitize := Processor(opts)
	p.Process = func(ctx context.Context, in upload.Info, body io.Reader) (io.Reader, upload.Info, error) {
		if base.Process != nil {
			var err error
			if body, in, err = base.Process(ctx, in, body); err != nil {
				return nil, upload.Info{}, err
			}
		}
		return sanitize(ctx, in, body)
	}
	check := Inspect(opts)
	p.Inspect = func(ctx context.Context, disk *filesystem.Disk, r upload.Result) error {
		if err := check(ctx, disk, r); err != nil {
			return err
		}
		if base.Inspect != nil {
			return base.Inspect(ctx, disk, r)
		}
		return nil
	}
	return p, p.Validate()
}
