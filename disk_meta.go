package filesystem

import (
	"context"
	"io"
	"maps"
	"strings"
	"time"
	"unicode"
)

// Tagger stores key-value tags with an object, separately from its bytes
// and metadata, so they can change without rewriting it (S3 object tags,
// Azure blob index tags). Lifecycle rules and cost reports often key off
// them. SetTags replaces every tag.
type Tagger interface {
	Tags(ctx context.Context, key string) (map[string]string, error)
	SetTags(ctx context.Context, key string, tags map[string]string) error
}

// Hold is an object's protection from deletion and overwrite, as object
// lock implements it (S3 Object Lock, GCS holds and retention, Azure
// immutability policies).
type Hold struct {
	// Legal protects the object until the hold is lifted, with no date.
	Legal bool
	// RetainUntil protects the object until this time; zero means none.
	// Backends refuse to shorten a retention already set in compliance
	// mode.
	RetainUntil time.Time
}

// Holder is implemented by drivers that declare the Hold capability, so
// compliance code can protect an object without reaching for the
// backend's SDK. SetHold replaces the whole Hold (both fields); the Disk
// cleans the key and counts it as a mutation, so a read-only disk refuses
// it before the driver is called. A backend that refuses to lift or
// shorten a hold returns its error unchanged.
type Holder interface {
	Hold(ctx context.Context, key string) (Hold, error)
	SetHold(ctx context.Context, key string, h Hold) error
}

// Version is one stored version of an object, newest first in the slice
// Versions returns.
type Version struct {
	// ID names the version to the backend.
	ID      string
	Size    int64
	ModTime time.Time
	ETag    string
	// Latest is the version a plain Read returns.
	Latest bool
	// DeleteMarker records a deletion rather than content.
	DeleteMarker bool
}

// VersionLister lists the versions a versioned bucket keeps of an object.
type VersionLister interface {
	Versions(ctx context.Context, key string) ([]Version, error)
}

// VersionReader reads one stored version of an object and makes a stored
// version the current content again, by the ID VersionLister reported.
// RestoreVersion keeps every version: the restored content becomes a new,
// latest version. A missing version, or a delete marker, is CodeNotFound.
type VersionReader interface {
	ReadVersion(ctx context.Context, key, id string) (io.ReadCloser, error)
	RestoreVersion(ctx context.Context, key, id string) error
}

// StorageClasser moves an existing object to another storage class (S3
// "STANDARD_IA" or "GLACIER", GCS "NEARLINE", Azure "Cool"), trading read
// cost and latency for storage cost. Class names are the backend's own and
// are passed through unchanged.
type StorageClasser interface {
	SetStorageClass(ctx context.Context, key, class string) error
}

// RestoreOptions configure Restore.
type RestoreOptions struct {
	// Days the restored copy stays readable before it returns to the
	// archive. Required, positive.
	Days int
}

// Restorer brings an archived object (S3 Glacier, Azure archive tier) back
// online for a while. The restore is asynchronous in the backend: Restore
// starts it, and reads succeed once the backend has finished.
type Restorer interface {
	Restore(ctx context.Context, key string, opts RestoreOptions) error
}

// Tags returns key's tags. Needs the Tags capability.
func (d *Disk) Tags(ctx context.Context, key string) (map[string]string, error) {
	var tags map[string]string
	err := d.objectOp(ctx, OpTags, key, d.caps.Tags, false, func(ctx context.Context, k string) error {
		var err error
		tags, err = d.driver.(Tagger).Tags(ctx, k)
		return err
	})
	return tags, err
}

// SetTags replaces key's tags with tags (nil or empty removes them all).
// A tag with an empty key, or a key or value holding a control character,
// is CodeInvalidArgument; backend limits (S3 allows ten tags) come back
// from the driver. Needs the Tags capability.
func (d *Disk) SetTags(ctx context.Context, key string, tags map[string]string) error {
	for k, v := range tags {
		if k == "" || strings.ContainsFunc(k+v, unicode.IsControl) {
			return annotate(Errorf(CodeInvalidArgument, "tag keys must be non-empty and tags free of control characters"), OpSetTags, d.name, key)
		}
	}
	tags = maps.Clone(tags)
	return d.objectOp(ctx, OpSetTags, key, d.caps.Tags, true, func(ctx context.Context, k string) error {
		return d.driver.(Tagger).SetTags(ctx, k, tags)
	})
}

// Hold returns key's hold. Needs the Hold capability.
func (d *Disk) Hold(ctx context.Context, key string) (Hold, error) {
	var h Hold
	err := d.objectOp(ctx, OpHold, key, d.caps.Hold, false, func(ctx context.Context, k string) error {
		var err error
		h, err = d.driver.(Holder).Hold(ctx, k)
		return err
	})
	return h, err
}

// SetHold sets key's hold: a legal hold, a retention date, both, or (the
// zero Hold) neither, where the backend allows lifting them. Needs the Hold
// capability.
func (d *Disk) SetHold(ctx context.Context, key string, h Hold) error {
	return d.objectOp(ctx, OpSetHold, key, d.caps.Hold, true, func(ctx context.Context, k string) error {
		return d.driver.(Holder).SetHold(ctx, k, h)
	})
}

// Versions returns key's stored versions, newest first. Needs the Versions
// capability (a versioned bucket).
func (d *Disk) Versions(ctx context.Context, key string) ([]Version, error) {
	var vs []Version
	err := d.objectOp(ctx, OpVersions, key, d.caps.Versions, false, func(ctx context.Context, k string) error {
		var err error
		vs, err = d.driver.(VersionLister).Versions(ctx, k)
		return err
	})
	return vs, err
}

// ReadVersion opens version id of key, an ID Versions reported. The caller
// must close the reader. An empty id is CodeInvalidArgument. Needs the
// VersionRead capability.
func (d *Disk) ReadVersion(ctx context.Context, key, id string) (io.ReadCloser, error) {
	k, err := d.CleanKey(key)
	if err != nil {
		return nil, annotate(err, OpReadVersion, d.name, key)
	}
	if id == "" {
		return nil, annotate(Errorf(CodeInvalidArgument, "empty version id"), OpReadVersion, d.name, k)
	}
	if !d.caps.VersionRead {
		return nil, d.unsupported(OpReadVersion, k)
	}
	var rc io.ReadCloser
	// Streaming: the reader outlives the call, so Timeout leaves it alone.
	op := &Op{Kind: OpReadVersion, Key: k, Replayable: true, Streaming: true}
	err = d.do(ctx, op, func(ctx context.Context) error {
		r, err := d.driver.(VersionReader).ReadVersion(ctx, k, id)
		if err != nil {
			if r != nil {
				_ = r.Close()
			}
			return err
		}
		rc = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return op.wrapReadCloser(rc), nil
}

// RestoreVersion makes version id of key its current content again, as a
// new latest version; no version is lost. An empty id is
// CodeInvalidArgument. Needs the VersionRead capability.
func (d *Disk) RestoreVersion(ctx context.Context, key, id string) error {
	if id == "" {
		return annotate(Errorf(CodeInvalidArgument, "empty version id"), OpRestoreVersion, d.name, key)
	}
	return d.objectOp(ctx, OpRestoreVersion, key, d.caps.VersionRead, true, func(ctx context.Context, k string) error {
		return d.driver.(VersionReader).RestoreVersion(ctx, k, id)
	})
}

// SetStorageClass moves key to storage class class, a name of the
// backend's own (S3 "STANDARD_IA", GCS "NEARLINE", Azure "Cool"); Stat
// then reports it in Entry.StorageClass. An empty class is
// CodeInvalidArgument; a class the backend does not know comes back from
// the driver. Needs the StorageClass capability.
func (d *Disk) SetStorageClass(ctx context.Context, key, class string) error {
	if class == "" || strings.ContainsFunc(class, unicode.IsControl) {
		return annotate(Errorf(CodeInvalidArgument, "storage class must be non-empty and free of control characters"), OpSetStorageClass, d.name, key)
	}
	return d.objectOp(ctx, OpSetStorageClass, key, d.caps.StorageClass, true, func(ctx context.Context, k string) error {
		return d.driver.(StorageClasser).SetStorageClass(ctx, k, class)
	})
}

// Restore starts bringing archived key back online for opts.Days. Needs the
// Restore capability; Days must be positive (CodeInvalidArgument).
func (d *Disk) Restore(ctx context.Context, key string, opts RestoreOptions) error {
	if opts.Days <= 0 {
		return annotate(Errorf(CodeInvalidArgument, "restore needs a positive number of days"), OpRestore, d.name, key)
	}
	return d.objectOp(ctx, OpRestore, key, d.caps.Restore, true, func(ctx context.Context, k string) error {
		return d.driver.(Restorer).Restore(ctx, k, opts)
	})
}

// objectOp runs one call about a single key behind capability capable:
// it cleans the key, refuses with CodeUnsupported without the
// capability, and runs call through the interceptors.
func (d *Disk) objectOp(ctx context.Context, kind OpKind, key string, capable, mutating bool, call func(ctx context.Context, k string) error) error {
	k, err := d.CleanKey(key)
	if err != nil {
		return annotate(err, kind, d.name, key)
	}
	if !capable {
		return d.unsupported(kind, k)
	}
	op := &Op{Kind: kind, Key: k, Mutating: mutating, Replayable: true}
	return d.do(ctx, op, func(ctx context.Context) error { return call(ctx, k) })
}
