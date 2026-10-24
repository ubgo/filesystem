package azure

import (
	"context"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/ubgo/filesystem"
)

// ObjectInfo is what Azure keeps about a blob beyond filesystem.Entry
// (which carries the tier as StorageClass): its archive state, version, protection and stored MD5. Azure checks
// each staged block's CRC64 when Config.ValidateCRC64 is on, but keeps no
// whole-blob CRC64, so the MD5 (present when the uploader set it, as
// single-shot uploads and many tools do) is the checksum to compare.
type ObjectInfo struct {
	// ArchiveStatus, while non-empty, says a rehydration is in progress
	// ("rehydrate-pending-to-hot").
	ArchiveStatus blob.ArchiveStatus
	// RehydratePriority of a rehydration in progress.
	RehydratePriority blob.RehydratePriority
	// VersionID is the current version, with blob versioning on.
	VersionID string
	// MD5 is the Content-MD5 stored with the blob; empty when the
	// uploader set none.
	MD5 []byte
	// Hold is the legal hold and immutability expiry (see Driver.Hold).
	Hold filesystem.Hold
	// RetentionMode is the immutability policy's mode, empty without one.
	RetentionMode blob.ImmutabilityPolicyMode
	// TierChanged is when the tier last changed.
	TierChanged time.Time
}

// ObjectInfo returns key's Azure-specific attributes. The key is cleaned
// like a Disk key, since this is called on the driver, past the Disk.
func (d *Driver) ObjectInfo(ctx context.Context, key string) (ObjectInfo, error) {
	k, err := filesystem.CleanKey(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := checkKey(k); err != nil {
		return ObjectInfo{}, err
	}
	p, err := d.properties(ctx, k)
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{
		ArchiveStatus:     blob.ArchiveStatus(val(p.ArchiveStatus)),
		RehydratePriority: blob.RehydratePriority(val(p.RehydratePriority)),
		VersionID:         val(p.VersionID),
		MD5:               p.ContentMD5,
		Hold:              holdOf(p),
		RetentionMode:     val(p.ImmutabilityPolicyMode),
		TierChanged:       val(p.AccessTierChangeTime),
	}, nil
}
