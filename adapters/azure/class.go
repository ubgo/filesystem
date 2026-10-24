package azure

import (
	"context"
	"slices"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/ubgo/filesystem"
)

// tier is Config.AccessTier for an SDK request, nil when unset.
func (d *Driver) tier() *blob.AccessTier {
	if d.cfg.AccessTier == "" {
		return nil
	}
	return &d.cfg.AccessTier
}

// SetStorageClass implements filesystem.StorageClasser with the blob's
// access tier: class is Hot, Cool, Cold or Archive (anything else is
// CodeInvalidArgument), changed in place without rewriting the blob. Moving
// to Archive takes the blob offline: reads then fail with ErrArchived
// until Restore brings it back. Moving an archived blob to an online tier
// is a rehydration with Config.RehydratePriority (or the call's
// RequestOptions.RehydratePriority), as Restore does.
func (d *Driver) SetStorageClass(ctx context.Context, key, class string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	tier := blob.AccessTier(class)
	if !slices.Contains(blob.PossibleAccessTierValues(), tier) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "azure: unknown access tier %q", class)
	}
	_, priority, err := d.rehydration(ctx, key)
	if err != nil {
		return err
	}
	var o *blob.SetTierOptions
	if priority != "" {
		o = &blob.SetTierOptions{RehydratePriority: &priority}
	}
	_, err = d.container.NewBlobClient(key).SetTier(ctx, tier, o)
	return mapErr(err)
}
