package s3

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// SetStorageClass implements filesystem.StorageClasser (declared with
// Config.StorageClasses) by copying the
// object onto itself in class, which keeps its bytes, metadata and tags (S3
// has no other way to change the class); the context's other write
// settings apply as for Copy. Moving into an archive class makes the
// object unreadable until it is restored; moving out of one needs a
// restore first. Objects over 5 GiB fail with S3's copy limit, and a class
// the store does not know comes back as S3's error.
//
// With Config.ObjectLock a held object is refused with CodePermission: the
// copy would replace the protected version with one that has no hold.
func (d *Driver) SetStorageClass(ctx context.Context, key, class string) error {
	if d.cfg.ObjectLock {
		if err := d.refuseHeld(ctx, key); err != nil {
			return err
		}
	}
	ws, err := d.writeSettings(ctx, key)
	if err != nil {
		return err
	}
	ws.class = types.StorageClass(class)
	_, err = d.copyObject(ctx, key, nil, key, ws)
	return err
}
