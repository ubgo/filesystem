package filesystem

import (
	"context"
	"errors"
	"slices"
)

// DeleteKeys deletes many keys: in few calls with the DeleteKeys
// capability (S3 deletes 1000 per request), otherwise one Delete per key.
// Missing keys succeed, as with Delete. Keys that could not be deleted,
// invalid ones included, come back in a *BatchError while every other key
// is deleted, so one bad key never stops a cleanup; nil means all are
// gone. Duplicate keys are deleted once.
func (d *Disk) DeleteKeys(ctx context.Context, keys []string) error {
	var failed []KeyError
	clean := make([]string, 0, len(keys))
	for _, k := range keys {
		c, err := d.CleanKey(k)
		if err != nil {
			failed = append(failed, KeyError{Key: k, Err: annotate(err, OpDeleteKeys, d.name, k)})
			continue
		}
		if !slices.Contains(clean, c) {
			clean = append(clean, c)
		}
	}
	if len(clean) > 0 {
		failed = append(failed, d.deleteKeys(ctx, clean)...)
	}
	if len(failed) > 0 {
		return &BatchError{Failures: failed}
	}
	return nil
}

// deleteKeys deletes cleaned keys and returns those that failed.
func (d *Disk) deleteKeys(ctx context.Context, keys []string) []KeyError {
	if !d.caps.DeleteKeys {
		var batch *BatchError
		if errors.As(Batch(ctx, keys, fallbackParallel, d.Delete), &batch) {
			return batch.Failures
		}
		return nil
	}
	deleter := d.driver.(KeysDeleter)
	op := &Op{Kind: OpDeleteKeys, Keys: keys, Mutating: true, Replayable: true}
	err := d.do(ctx, op, func(ctx context.Context) error { return deleter.DeleteKeys(ctx, keys) })
	if err == nil {
		return nil
	}
	var batch *BatchError
	if errors.As(err, &batch) {
		return batch.Failures // annotated per key by d.do
	}
	// The call failed as a whole: no key is known to be deleted.
	out := make([]KeyError, len(keys))
	for i, k := range keys {
		out[i] = KeyError{Key: k, Err: err}
	}
	return out
}
