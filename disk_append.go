package filesystem

import (
	"context"
	"crypto/sha256"
	"io"
	"time"
)

// AppendOptions control an append.
type AppendOptions struct {
	// Visibility of an object Append creates; empty means the disk's
	// default. An existing object keeps its own.
	Visibility Visibility
	// MaxSize, when positive, caps the object's size after the append: an
	// append that would take it past MaxSize is refused with CodeTooLarge
	// and appends nothing. A log or export then cannot grow without bound
	// however many appends arrive.
	MaxSize int64
}

// Appender adds bytes to the end of an object, creating it when missing.
// Contract, checked by the conformance suite: a body that fails to read
// appends nothing (the object is as it was), two appends to one key never
// interleave their bytes, and an append that would take the object past a
// positive opts.MaxSize is refused with CodeTooLarge without appending
// (the check and the append are one step, so concurrent appends cannot
// overshoot together).
type Appender interface {
	Append(ctx context.Context, key string, body io.Reader, opts AppendOptions) (Entry, error)
}

// ModTimeSetter sets an object's modification time, for tools that sync
// or archive by date (rsync-style mirroring, restoring a backup).
type ModTimeSetter interface {
	SetModTime(ctx context.Context, key string, t time.Time) error
}

// Append adds body to the end of key, creating the object (with
// opts.Visibility, or the disk's default) when it does not exist, and
// returns the object's entry afterwards: logs, audit trails, chunked
// exports. With opts.MaxSize the object never grows past it: a body that
// alone exceeds it is cut off as it streams, and the driver refuses an
// append that would exceed it, both with CodeTooLarge and nothing appended. Needs the Append
// capability (local, memory, SFTP, Azure append blobs); object stores
// without appends answer CodeUnsupported. The body is a stream, so an
// append is never retried. Size and hash in the entry are the whole
// object's as the driver reports them, not the appended part's.
func (d *Disk) Append(ctx context.Context, key string, body io.Reader, opts AppendOptions) (Entry, error) {
	k, err := d.CleanKey(key)
	if err != nil {
		return Entry{}, annotate(err, OpAppend, d.name, key)
	}
	if body == nil {
		return Entry{}, annotate(Errorf(CodeInvalidArgument, "nil body"), OpAppend, d.name, k)
	}
	if opts.MaxSize < 0 {
		return Entry{}, annotate(Errorf(CodeInvalidArgument, "negative MaxSize"), OpAppend, d.name, k)
	}
	switch {
	case opts.Visibility == "":
		opts.Visibility = d.defaultVisibility
	case !opts.Visibility.Valid():
		return Entry{}, annotate(Errorf(CodeInvalidArgument, "unknown visibility %q", opts.Visibility), OpAppend, d.name, k)
	}
	if !d.caps.Append {
		return Entry{}, d.unsupported(OpAppend, k)
	}
	appender := d.driver.(Appender)
	capped := &meteredReader{r: body, max: opts.MaxSize, hash: sha256.New()}
	var e Entry
	op := &Op{Kind: OpAppend, Key: k, Mutating: true}
	err = d.do(ctx, op, func(ctx context.Context) error {
		var err error
		e, err = appender.Append(ctx, k, op.wrapBody(capped), opts)
		if capped.tooLarge {
			// A driver must append nothing when its body fails; the cap
			// is that failure, reported as what it is.
			return Errorf(CodeTooLarge, "append exceeds %d bytes", opts.MaxSize)
		}
		return err
	})
	if err != nil {
		return Entry{}, err
	}
	e.Key = k
	return e, nil
}

// SetModTime sets key's modification time to t, which Stat then reports
// (to the backend's precision, a second at worst). Needs the SetModTime
// capability (local, memory, SFTP); object stores, which stamp the upload
// time themselves, answer CodeUnsupported. A zero t is
// CodeInvalidArgument.
func (d *Disk) SetModTime(ctx context.Context, key string, t time.Time) error {
	if t.IsZero() {
		return annotate(Errorf(CodeInvalidArgument, "zero modification time"), OpSetModTime, d.name, key)
	}
	return d.objectOp(ctx, OpSetModTime, key, d.caps.SetModTime, true, func(ctx context.Context, k string) error {
		return d.driver.(ModTimeSetter).SetModTime(ctx, k, t)
	})
}
