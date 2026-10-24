package filesystem

import (
	"context"
	"io"
	"time"
)

// ReadOptions make a read conditional, as HTTP's conditional GET: the body
// is returned only when the object differs from the caller's copy.
type ReadOptions struct {
	// IfNoneMatch is the ETag of the caller's copy; "*" matches any
	// existing object. When it matches, the read answers CodeNotModified.
	// It takes precedence over IfModifiedSince, as in HTTP.
	IfNoneMatch string
	// IfModifiedSince answers CodeNotModified when the object was not
	// modified after it (compared to the second, as HTTP dates are).
	IfModifiedSince time.Time
}

// anyETag is the If-None-Match value that matches every existing object.
const anyETag = "*"

// ConditionalReader answers a conditional read in the backend (S3, GCS and
// Azure send the conditions with the GET), returning CodeNotModified and
// no body when the condition holds.
type ConditionalReader interface {
	ReadIf(ctx context.Context, key string, opts ReadOptions) (io.ReadCloser, Entry, error)
}

// ReadIf reads key unless the caller's copy is current: it answers
// CodeNotModified, with no body, when opts.IfNoneMatch matches the
// object's ETag, or (with no IfNoneMatch) when the object was not modified
// after opts.IfModifiedSince. It returns the object's entry with the body,
// for the ETag and time the caller keeps for its next read. Drivers with
// the ConditionalRead capability decide in the backend; on others the Disk
// compares against a Stat before reading, which a concurrent overwrite
// between the two can outrun (the body is then newer than the entry). An
// object without an ETag never matches IfNoneMatch.
func (d *Disk) ReadIf(ctx context.Context, key string, opts ReadOptions) (io.ReadCloser, Entry, error) {
	k, err := d.CleanKey(key)
	if err != nil {
		return nil, Entry{}, annotate(err, OpRead, d.name, key)
	}
	if d.caps.ConditionalRead {
		reader := d.driver.(ConditionalReader)
		var rc io.ReadCloser
		var e Entry
		op := &Op{Kind: OpRead, Key: k, Replayable: true, Streaming: true}
		err := d.do(ctx, op, func(ctx context.Context) error {
			r, got, err := reader.ReadIf(ctx, k, opts)
			if err != nil {
				// As in ReadRange: a reader returned with an error, or
				// by a failed attempt before a retry, must not leak.
				if r != nil {
					_ = r.Close()
				}
				return err
			}
			rc, e = r, got
			return nil
		})
		if err != nil {
			op.closed()
			return nil, Entry{}, err
		}
		e.Key = k
		return op.wrapReadCloser(rc), e, nil
	}
	e, err := d.Stat(ctx, k)
	if err != nil {
		return nil, Entry{}, err
	}
	if NotModified(e, opts) {
		return nil, Entry{}, annotate(Errorf(CodeNotModified, "object unchanged"), OpRead, d.name, k)
	}
	rc, err := d.Read(ctx, k)
	if err != nil {
		return nil, Entry{}, err
	}
	return rc, e, nil
}

// NotModified reports whether opts say the caller's copy of the object e
// describes is current, by the HTTP rules: If-None-Match first (an exact
// ETag, or "*" for any object), else If-Modified-Since to the second. It
// is exported for drivers that implement ConditionalReader over a Stat.
func NotModified(e Entry, opts ReadOptions) bool {
	if opts.IfNoneMatch != "" {
		return opts.IfNoneMatch == anyETag || (e.ETag != "" && trimETag(opts.IfNoneMatch) == trimETag(e.ETag))
	}
	return !opts.IfModifiedSince.IsZero() && !e.ModTime.Truncate(time.Second).After(opts.IfModifiedSince.Truncate(time.Second))
}

// trimETag drops the quotes and weak prefix HTTP puts around an ETag, so
// a value copied from a header compares with the bare one drivers report.
func trimETag(s string) string {
	if len(s) > 2 && s[:2] == "W/" {
		s = s[2:]
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	return s
}
