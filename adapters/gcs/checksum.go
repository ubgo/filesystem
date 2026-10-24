package gcs

import (
	"context"
	"strconv"
	"time"

	"github.com/ubgo/filesystem"
)

// ObjectInfo is what GCS keeps about an object beyond filesystem.Entry
// (which carries the storage class): its generation, retention state and the checksums GCS
// computed over the stored bytes. The SDK sends a CRC32C of every upload
// and GCS refuses a body that does not match it, and full reads are
// checked against the stored CRC32C by the SDK; these fields let an
// application compare against a checksum it holds (a manifest, another
// backend's copy) without reading the object.
type ObjectInfo struct {
	// VersionID is the current generation, as Versions reports it.
	VersionID string
	// CRC32C is the Castagnoli CRC-32 of the stored bytes. Always set.
	CRC32C uint32
	// MD5 is the MD5 of the stored bytes; empty for composite objects,
	// which have only a CRC32C.
	MD5 []byte
	// Hold is the temporary hold and retention date (see Driver.Hold).
	Hold filesystem.Hold
	// EventBasedHold is set while a bucket's event-based hold applies.
	EventBasedHold bool
	// RetentionMode is the mode of the retention date, empty without one.
	RetentionMode RetentionMode
	// Created is when this generation was written.
	Created time.Time
}

// ObjectInfo returns key's GCS-specific attributes. The key is cleaned
// like a Disk key, since this is called on the driver, past the Disk.
func (d *Driver) ObjectInfo(ctx context.Context, key string) (ObjectInfo, error) {
	k, err := filesystem.CleanKey(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	a, err := d.object(k).Attrs(ctx)
	if err != nil {
		return ObjectInfo{}, mapErr(err)
	}
	info := ObjectInfo{
		VersionID:      strconv.FormatInt(a.Generation, 10),
		CRC32C:         a.CRC32C,
		MD5:            a.MD5,
		Hold:           holdOf(a),
		EventBasedHold: a.EventBasedHold,
		Created:        a.Created,
	}
	if a.Retention != nil {
		info.RetentionMode = RetentionMode(a.Retention.Mode)
	}
	return info, nil
}
