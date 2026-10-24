package gcs

import (
	"context"
	"slices"
	"time"

	"cloud.google.com/go/storage"

	"github.com/ubgo/filesystem"
)

// RequestOptions are GCS settings for the operations on one key, for what
// filesystem.WriteOptions has no portable field for, mirroring the s3
// adapter's RequestOptions. Attach them with WithRequestOptions and call
// the Disk as usual: the options travel through the Disk (key cleaning, a
// scoped disk's prefix, interceptors) to the driver, which is why they
// ride on the context rather than on a driver method. They apply only to
// that exact key, so a context reused for another write (a thumbnail, the
// same name in another directory) never inherits another tenant's key.
// Every field is optional and overrides the disk's Config for those calls
// only.
//
// Why: Config.KMSKeyFor picks a key by the object's name, which serves a
// tenant layout known in advance; a key chosen per request (a tenant's
// key read from the database, a customer moved to its own key) needs a
// per-call option, as s3's SSEKMSKeyID gives.
//
// It is this package's one escape hatch for per-call GCS features; the
// native client (filesystem.As[*storage.Client]) is the one for
// everything else.
type RequestOptions struct {
	// KMSKeyName is the Cloud KMS key a written or copied object (or a
	// presigned PUT or form upload for it, a storage-class rewrite, a
	// version restore) is encrypted with, overriding Config.KMSKeyFor and
	// Config.KMSKeyName. Exclusive with Config.CustomerKey
	// (CodeInvalidArgument otherwise).
	KMSKeyName string
	// StorageClass of a written or copied object, overriding
	// Config.StorageClass; one of StorageClassValues. A presigned PUT
	// signs it (x-goog-storage-class), so the client must send it.
	StorageClass StorageClass
	// RetainUntil gives a written or copied object a retention date, in
	// Config.RetentionMode, set in the same request that creates it, so
	// the object is never unprotected (Disk.SetHold afterwards leaves a
	// window between the write and the hold). It only lengthens what
	// Config.RetainFor gives: the later of the two dates wins, so a call
	// cannot shorten a disk's retention. It must be after now (by
	// Config.Clock) and needs a bucket with object retention enabled.
	RetainUntil time.Time
	// TemporaryHold places a temporary hold on a written or copied object
	// in the same request (Disk.SetHold with Hold.Legal releases it later).
	// While it holds, GCS refuses to delete or replace the object.
	TemporaryHold bool
	// CustomTime sets the object's Custom-Time on a write or copy, the date
	// bucket lifecycle rules count daysSinceCustomTime from (an invoice's
	// due date, a recording's event time). GCS never lets it move back once
	// set.
	CustomTime time.Time
}

// objectLock reports whether o asks for a retention date, a hold or a
// Custom-Time, none of which a signed URL or form can pin.
func (o RequestOptions) objectLock() bool {
	return !o.RetainUntil.IsZero() || o.TemporaryHold || !o.CustomTime.IsZero()
}

type requestOptionsKey struct{}

// keyedOptions are RequestOptions bound to the key they are for.
type keyedOptions struct {
	key  string
	opts RequestOptions
}

// WithRequestOptions returns a context carrying o for the calls on disk's
// key made with it (the destination of a Copy); a call on any other key,
// however similar, ignores them. It binds the options to the key the
// driver will see (disk.DriverKey: cleaned, under a scoped disk's prefix),
// so a caller never joins prefixes and cannot bind them to the wrong key;
// an invalid key is returned as the error, with ctx unchanged. For a form
// upload under a key prefix, bind them to the form's key,
// "prefix/${filename}". The latest call wins: options are not merged.
// They are not consumed, so a retried call gets them again.
func WithRequestOptions(ctx context.Context, disk *filesystem.Disk, key string, o RequestOptions) (context.Context, error) {
	k, err := disk.DriverKey(key)
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, requestOptionsKey{}, keyedOptions{key: k, opts: o}), nil
}

// requestOptions returns the options ctx carries for driver key key, or
// the zero value.
func requestOptions(ctx context.Context, key string) RequestOptions {
	k, ok := ctx.Value(requestOptionsKey{}).(keyedOptions)
	if !ok || k.key == "" || key != k.key {
		return RequestOptions{}
	}
	return k.opts
}

// callSettings are the call's RequestOptions for key, checked: a KMS key
// on a CSEK disk, an unknown storage class and a RetainUntil not after now
// are CodeInvalidArgument.
func (d *Driver) callSettings(ctx context.Context, key string) (RequestOptions, error) {
	o := requestOptions(ctx, key)
	if o.KMSKeyName != "" && !d.cfg.CustomerKey.IsZero() {
		return RequestOptions{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: a KMS key is exclusive with Config.CustomerKey")
	}
	if o.StorageClass != "" && !slices.Contains(StorageClassValues, o.StorageClass) {
		return RequestOptions{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: unknown storage class %q", o.StorageClass)
	}
	if !o.RetainUntil.IsZero() && !o.RetainUntil.After(d.now()) {
		return RequestOptions{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: RetainUntil %s is not in the future", o.RetainUntil.Format(time.RFC3339))
	}
	return o, nil
}

// presignSettings are callSettings for a presigned PUT or form, which
// refuse a retention date, a hold or a Custom-Time (CodeUnsupported):
// GCS has no signed field for them, and dropping them silently would hand
// out an upload that lands unprotected.
func (d *Driver) presignSettings(ctx context.Context, key string) (RequestOptions, error) {
	o, err := d.callSettings(ctx, key)
	if err != nil {
		return RequestOptions{}, err
	}
	if o.objectLock() {
		return RequestOptions{}, filesystem.Errorf(filesystem.CodeUnsupported, "gcs: a presigned upload cannot pin RetainUntil, TemporaryHold or CustomTime; write through the disk")
	}
	return o, nil
}

// writeRetention is the retention a write or copy sets: the later of
// Config.RetainFor's date and the call's RetainUntil, or nil with neither.
func (d *Driver) writeRetention(o RequestOptions) *storage.ObjectRetention {
	r := d.retention()
	if o.RetainUntil.IsZero() || (r != nil && !o.RetainUntil.After(r.RetainUntil)) {
		return r
	}
	return &storage.ObjectRetention{Mode: string(d.retentionMode()), RetainUntil: o.RetainUntil}
}

// kmsKey is the Cloud KMS key for key: the call's RequestOptions, else
// Config.KMSKeyFor's choice, else Config.KMSKeyName (empty for the
// bucket's default).
func (d *Driver) kmsKey(o RequestOptions, key string) string {
	if o.KMSKeyName != "" {
		return o.KMSKeyName
	}
	if d.cfg.KMSKeyFor != nil {
		if k := d.cfg.KMSKeyFor(key); k != "" {
			return k
		}
	}
	return d.cfg.KMSKeyName
}

// storageClass is the class a write or copy of key gets: the call's,
// else Config.StorageClass ("" for the bucket's default).
func (d *Driver) storageClass(o RequestOptions) string {
	if o.StorageClass != "" {
		return string(o.StorageClass)
	}
	return string(d.cfg.StorageClass)
}
