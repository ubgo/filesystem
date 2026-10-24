package filesystem

import (
	"context"
	"sync"
)

// fallbackParallel is how many keys the Disk's own fallbacks (DeleteKeys and
// DeletePrefix on a driver without the bulk capability) work on at once:
// enough to hide one round trip per key on a remote backend, few enough
// that a large cleanup does not flood it. A Limit interceptor on the disk
// still caps the total.
const fallbackParallel = 8

// Batch runs fn for every key with at most parallel calls at a time and
// collects the failures: nil when every call succeeded, otherwise a
// *BatchError listing each failed key with its error, in the order of keys.
// It is the helper behind the Disk's per-key fallbacks, and the shape to
// use for any per-key work an application fans out (copying a tree,
// re-encoding every image under a prefix).
//
// A parallel below 1 runs one call at a time. Keys are passed as given,
// duplicates included. Once ctx ends no further call starts, and every key
// not yet started fails with the context's error, so the result still
// names every key that was not processed.
func Batch(ctx context.Context, keys []string, parallel int, fn func(ctx context.Context, key string) error) error {
	parallel = max(parallel, 1)
	errs := make([]error, len(keys))
	slots := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, k := range keys {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}
		if err := ctx.Err(); err != nil {
			errs[i] = NewError(CodeUnavailable, err)
			continue
		}
		wg.Go(func() {
			defer func() { <-slots }()
			errs[i] = fn(ctx, k)
		})
	}
	wg.Wait()
	var failed []KeyError
	for i, err := range errs {
		if err != nil {
			failed = append(failed, KeyError{Key: keys[i], Err: err})
		}
	}
	if len(failed) > 0 {
		return &BatchError{Failures: failed}
	}
	return nil
}
