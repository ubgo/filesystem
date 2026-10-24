package azure

import (
	"context"
	"errors"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/ubgo/filesystem"
)

// ErrArchived is matched (errors.Is) by the error of a read or Stat-like
// call on a blob in the Archive tier or still being rehydrated from it;
// its code is CodeConflict, since the blob exists but its state forbids
// the call until a Restore finishes (hours, depending on the priority).
var ErrArchived = errors.New("azure: blob is archived or being rehydrated")

// Restore implements filesystem.Restorer by rehydrating an archived blob:
// it moves it to Config.RehydrateTier (Hot when empty) with
// Config.RehydratePriority, or to the call's RequestOptions'. Azure rehydrates to a tier for good rather
// than for a number of days, so opts.Days (still required by the Disk) is
// not sent; move the blob back with SetAccessTier when done. Restore
// returns once Azure accepted the request; ObjectInfo.ArchiveStatus shows
// the progress, and reads fail with ErrArchived until it finishes.
func (d *Driver) Restore(ctx context.Context, key string, _ filesystem.RestoreOptions) error {
	if err := checkKey(key); err != nil {
		return err
	}
	tier, priority, err := d.rehydration(ctx, key)
	if err != nil {
		return err
	}
	if tier == "" {
		tier = blob.AccessTierHot
	}
	if priority == "" {
		priority = blob.RehydratePriorityStandard
	}
	_, err = d.container.NewBlobClient(key).SetTier(ctx, tier, &blob.SetTierOptions{RehydratePriority: &priority})
	return mapErr(err)
}
