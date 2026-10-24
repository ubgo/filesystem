package azure

import (
	"context"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/ubgo/filesystem"
)

func (d *Driver) retentionMode() blob.ImmutabilityPolicySetting {
	if d.cfg.RetentionMode == "" {
		return blob.ImmutabilityPolicySettingUnlocked
	}
	return d.cfg.RetentionMode
}

// retain gives a freshly stored blob its protection: the policy expiring
// at ws.retainUntil (RequestOptions.RetainUntil), else Config.RetainFor's,
// and the legal hold ws asks for; with none of them it does nothing. The
// blob is already stored when it runs, so a failure says so, keeping the
// cause's code: the caller must not take the error for a write that
// stored nothing.
func (d *Driver) retain(ctx context.Context, key string, ws writeSettings) error {
	until := ws.retainUntil
	if until.IsZero() && d.cfg.RetainFor > 0 {
		until = d.now().Add(d.cfg.RetainFor)
	}
	b := d.container.NewBlobClient(key)
	if !until.IsZero() {
		mode := d.retentionMode()
		if _, err := b.SetImmutabilityPolicy(ctx, until, &blob.SetImmutabilityPolicyOptions{Mode: &mode}); err != nil {
			return unprotected(key, "immutability policy", err)
		}
	}
	if ws.legalHold {
		if _, err := b.SetLegalHold(ctx, true, nil); err != nil {
			return unprotected(key, "legal hold", err)
		}
	}
	return nil
}

// unprotected is the error of a protection that failed on a blob that is
// already stored, keeping the cause's code.
func unprotected(key, what string, err error) error {
	return filesystem.NewError(filesystem.CodeOf(mapErr(err)), fmt.Errorf("azure: %q was stored, but setting its %s failed, so it is unprotected (re-apply with SetHold): %w", key, what, err))
}

// Hold implements filesystem.Holder: Legal is the blob's legal hold,
// RetainUntil its immutability policy's expiry. Both need a container (or
// account) with version-level immutability support.
func (d *Driver) Hold(ctx context.Context, key string) (filesystem.Hold, error) {
	if err := checkKey(key); err != nil {
		return filesystem.Hold{}, err
	}
	p, err := d.properties(ctx, key)
	if err != nil {
		return filesystem.Hold{}, err
	}
	return holdOf(p), nil
}

// holdOf reads the legal hold and immutability expiry from properties.
func holdOf(p blob.GetPropertiesResponse) filesystem.Hold {
	return filesystem.Hold{Legal: val(p.LegalHold), RetainUntil: val(p.ImmutabilityPolicyExpiresOn)}
}

// SetHold implements filesystem.Holder. The legal hold is set or cleared
// as asked. A RetainUntil different from the blob's current expiry sets
// the policy in Config.RetentionMode, and a zero one deletes an Unlocked
// policy; Azure refuses to shorten or delete a Locked one
// (CodePermission). While either protection holds, Delete and overwrites
// fail with CodePermission.
func (d *Driver) SetHold(ctx context.Context, key string, h filesystem.Hold) error {
	cur, err := d.Hold(ctx, key)
	if err != nil {
		return err
	}
	b := d.container.NewBlobClient(key)
	if cur.Legal != h.Legal {
		if _, err := b.SetLegalHold(ctx, h.Legal, nil); err != nil {
			return mapErr(err)
		}
	}
	switch {
	case h.RetainUntil.Equal(cur.RetainUntil):
	case h.RetainUntil.IsZero():
		_, err = b.DeleteImmutabilityPolicy(ctx, nil)
	default:
		mode := d.retentionMode()
		_, err = b.SetImmutabilityPolicy(ctx, h.RetainUntil.Truncate(time.Second), &blob.SetImmutabilityPolicyOptions{Mode: &mode})
	}
	return mapErr(err)
}
