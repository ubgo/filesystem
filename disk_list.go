package filesystem

import (
	"context"
	"iter"
)

// ListPage returns one page of the entries under dir (see ListOptions). dir
// is cleaned like a key, except that "" means the root.
func (d *Disk) ListPage(ctx context.Context, dir string, opts ListOptions) (Page, error) {
	clean, err := d.cleanDir(dir)
	if err != nil {
		return Page{}, annotate(err, OpList, d.name, dir)
	}
	if opts.Limit < 0 {
		return Page{}, annotate(Errorf(CodeInvalidArgument, "negative list limit %d", opts.Limit), OpList, d.name, clean)
	}
	if opts.Limit == 0 {
		opts.Limit = DefaultPageSize
	}
	var page Page
	op := &Op{Kind: OpList, Key: clean, Replayable: true}
	err = d.do(ctx, op, func(ctx context.Context) error {
		var err error
		page, err = d.driver.List(ctx, clean, opts)
		return err
	})
	if err != nil {
		return Page{}, err
	}
	return page, nil
}

// List iterates over every entry under dir, fetching pages lazily; each page
// is one intercepted (and so retryable) driver call. Breaking out of the
// loop stops fetching. A failure is yielded once as (Entry{}, err) and ends
// the iteration.
//
//	for e, err := range disk.List(ctx, "avatars", filesystem.ListOptions{Recursive: true}) {
//		if err != nil { return err }
//		…
//	}
func (d *Disk) List(ctx context.Context, dir string, opts ListOptions) iter.Seq2[Entry, error] {
	return func(yield func(Entry, error) bool) {
		for {
			page, err := d.ListPage(ctx, dir, opts)
			if err != nil {
				yield(Entry{}, err)
				return
			}
			for _, e := range page.Entries {
				if !yield(e, nil) {
					return
				}
			}
			if page.NextCursor == "" {
				return
			}
			opts.Cursor = page.NextCursor
		}
	}
}

// DeletePrefix deletes every key under dir. With the DeletePrefix capability
// the backend does it in bulk; otherwise the Disk lists and deletes key by
// key. dir must not be the root: emptying a whole disk is never a prefix
// operation, so it is refused with CodeInvalidArgument.
func (d *Disk) DeletePrefix(ctx context.Context, dir string) error {
	clean, err := d.cleanDir(dir)
	if err != nil {
		return annotate(err, OpDeletePrefix, d.name, dir)
	}
	if clean == "" {
		return annotate(Errorf(CodeInvalidArgument, "refusing to delete the root of the disk"), OpDeletePrefix, d.name, clean)
	}
	if d.caps.DeletePrefix {
		deleter := d.driver.(PrefixDeleter)
		op := &Op{Kind: OpDeletePrefix, Key: clean, Mutating: true, Replayable: true}
		return d.do(ctx, op, func(ctx context.Context) error {
			return deleter.DeletePrefix(ctx, clean)
		})
	}
	// Page by page: each page's keys are deleted in parallel, and the
	// listing resumes after the last key, which deleting does not move.
	opts := ListOptions{Recursive: true}
	for {
		page, err := d.ListPage(ctx, clean, opts)
		if err != nil {
			return err
		}
		// A recursive listing never yields IsDir entries (Driver contract).
		keys := make([]string, len(page.Entries))
		for i, e := range page.Entries {
			keys[i] = e.Key
		}
		if err := Batch(ctx, keys, fallbackParallel, d.Delete); err != nil {
			return err
		}
		if page.NextCursor == "" {
			return nil
		}
		opts.Cursor = page.NextCursor
	}
}
