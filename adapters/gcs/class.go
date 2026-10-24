package gcs

import (
	"context"
	"net/http"

	"cloud.google.com/go/storage"

	"github.com/ubgo/filesystem"
)

// StorageClass is a GCS storage class. Colder classes cost less to keep
// and more to read, with a minimum storage duration; every class is read
// directly, without a restore step, so the driver declares no Restore.
type StorageClass string

const (
	// ClassStandard is for data read often.
	ClassStandard StorageClass = "STANDARD"
	// ClassNearline is for data read about once a month (30-day minimum).
	ClassNearline StorageClass = "NEARLINE"
	// ClassColdline is for data read about once a quarter (90-day minimum).
	ClassColdline StorageClass = "COLDLINE"
	// ClassArchive is for data read less than once a year (365-day minimum).
	ClassArchive StorageClass = "ARCHIVE"
)

// StorageClassValues lists every StorageClass.
var StorageClassValues = []StorageClass{ClassStandard, ClassNearline, ClassColdline, ClassArchive}

// SetStorageClass implements filesystem.StorageClasser; class is a GCS
// class name (StorageClassValues). GCS changes a class only by rewriting
// the object, so this rewrites it in place inside GCS (no bytes pass
// through the application), keeping its content type, headers, metadata
// and, in ACL mode, its ACL. The rewrite is conditioned on the generation
// and metageneration it read, so an object replaced meanwhile is not
// overwritten with the old bytes, and a metadata change made meanwhile is
// not reverted by the copy of the old metadata (both CodeConflict). The
// object keeps its retention date; one without gets Config.RetainFor, as
// any new generation does. On a versioned bucket the previous generation
// becomes a noncurrent version.
func (d *Driver) SetStorageClass(ctx context.Context, key, class string) error {
	call, err := d.callSettings(ctx, key)
	if err != nil {
		return err
	}
	src := d.object(key)
	a, err := src.Attrs(ctx)
	if err != nil {
		return mapErr(err)
	}
	dst := d.object(key).If(storage.Conditions{GenerationMatch: a.Generation, MetagenerationMatch: a.Metageneration})
	c := dst.CopierFrom(src.Generation(a.Generation))
	c.StorageClass = class
	c.ContentType = a.ContentType
	c.ContentEncoding = a.ContentEncoding
	c.ContentDisposition = a.ContentDisposition
	c.ContentLanguage = a.ContentLanguage
	c.CacheControl = a.CacheControl
	c.Metadata = a.Metadata
	c.Retention = a.Retention
	if c.Retention == nil {
		c.Retention = d.retention()
	}
	c.DestinationKMSKeyName = d.kmsKey(call, key)
	if d.cfg.Visibility == VisibilityACL {
		c.ACL = a.ACL
	}
	_, err = c.Run(ctx)
	if statusOf(err) == http.StatusPreconditionFailed {
		return filesystem.NewError(filesystem.CodeConflict, err)
	}
	return mapErr(err)
}
