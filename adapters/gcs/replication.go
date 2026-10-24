package gcs

import (
	"context"

	"cloud.google.com/go/storage"

	"github.com/ubgo/filesystem"
)

// ReplicationMode is how fast a dual-region or multi-region bucket copies
// new objects to its second region (GCS calls it the recovery point
// objective). Which regions a bucket spans is fixed when it is created
// (its location, or storage.BucketAttrs.CustomPlacementConfig) and
// cannot be changed afterwards; the mode is the part that can.
type ReplicationMode string

// The replication modes GCS offers.
const (
	// ReplicationDefault copies most new objects within an hour, all
	// within twelve.
	ReplicationDefault ReplicationMode = "DEFAULT"
	// ReplicationTurbo (turbo replication, billed extra) copies every new
	// object within fifteen minutes. Dual-region buckets only.
	ReplicationTurbo ReplicationMode = "ASYNC_TURBO"
)

// SetReplication sets the bucket's replication mode. Like SetCORS it is a
// setup step run with credentials allowed storage.buckets.update. GCS
// refuses ReplicationTurbo on a bucket that is not dual-region; an
// unknown mode is refused before any request.
func SetReplication(ctx context.Context, client *storage.Client, bucket string, mode ReplicationMode) error {
	rpo := mode.rpo()
	if rpo == storage.RPOUnknown {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: replication mode %q is not DEFAULT or ASYNC_TURBO", mode)
	}
	_, err := client.Bucket(bucket).Update(ctx, storage.BucketAttrsToUpdate{RPO: rpo})
	return mapErr(err)
}

// Replication reads the bucket's replication mode back. A single-region
// bucket, which has none, reads as the empty mode. An RPO the SDK reports
// that is neither of the modes (a value added in a later SDK) is a
// CodeUnsupported error naming it, never read as "none". The SDK itself
// turns a mode it does not know into storage.RPOUnknown, the same value
// as no mode, so that case cannot be told apart from here.
func Replication(ctx context.Context, client *storage.Client, bucket string) (ReplicationMode, error) {
	attrs, err := client.Bucket(bucket).Attrs(ctx)
	if err != nil {
		return "", mapErr(err)
	}
	return modeOf(attrs.RPO)
}

// modeOf is the mode for an SDK RPO value: empty for none, an error for a
// value this package does not know.
func modeOf(rpo storage.RPO) (ReplicationMode, error) {
	if rpo == storage.RPOUnknown {
		return "", nil
	}
	for _, mode := range []ReplicationMode{ReplicationDefault, ReplicationTurbo} {
		if rpo == mode.rpo() {
			return mode, nil
		}
	}
	return "", filesystem.Errorf(filesystem.CodeUnsupported, "gcs: replication RPO %v is not a mode ReplicationMode represents", rpo)
}

// rpo is the SDK's value for m, RPOUnknown for a mode GCS does not have.
func (m ReplicationMode) rpo() storage.RPO {
	switch m {
	case ReplicationDefault:
		return storage.RPODefault
	case ReplicationTurbo:
		return storage.RPOAsyncTurbo
	}
	return storage.RPOUnknown
}
