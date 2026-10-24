package archive

import (
	"context"
	"errors"
	"io"

	"github.com/ubgo/filesystem"
)

// List walks the archive in r (size bytes) and returns every entry, nested
// archives' entries included, without storing anything. Every limit and
// name rule applies as for Extract, so List is the check to run before
// trusting an archive. Input that is not an archive is ErrFormat.
func List(ctx context.Context, r io.ReaderAt, size int64, limits Limits) ([]Entry, error) {
	var out []Entry
	err := walkArchive(ctx, r, size, limits, func(_ context.Context, e Entry, content io.Reader) error {
		if err := counted(&e, content); err != nil {
			return err
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// counted reads content to its end and sets e.Size to what it held. A
// read that fails is classified like every other read of the walk: a
// limit or the context ending keeps its own error, a broken stream (a
// truncated or corrupt entry) is ErrFormat.
func counted(e *Entry, content io.Reader) error {
	if content == nil {
		return nil
	}
	n, err := io.Copy(io.Discard, content)
	if err != nil {
		return classify(err)
	}
	e.Size = n
	return nil
}

// ExtractOptions control Extract.
type ExtractOptions struct {
	// Limits bound the archive; the zero value is the defaults.
	Limits Limits
	// Overwrite lets Extract replace objects that already exist under the
	// prefix. Off by default, so an archive can never replace a file the
	// application stored there (an avatar, a configuration file) by
	// naming it.
	Overwrite bool
	// Skip, when set, is asked about every top-level file entry before it
	// is stored; true leaves the entry out of the disk and of the returned
	// list. Use it for archive debris such as __MACOSX/ and .DS_Store. A
	// skipped entry is still read through the walk, so every limit and
	// name rule applies to it as to a stored one: Skip can only store
	// less, never let an unsafe archive through. Nil stores every file.
	Skip func(Entry) bool
}

// Extract stores every file of the archive in r (size bytes) on disk under
// prefix, at filesystem.JoinKey(prefix, Entry.Name), and returns the
// entries it stored. Directories create nothing (object stores have none).
// A nested archive is stored as the file it is, after its own entries have
// passed the limits; it is not expanded.
//
// A key that already exists is refused with CodeExists unless
// opts.Overwrite is set: atomically on disks with the ConditionalWrite
// capability (WriteOptions.IfNotExists), by a Stat before the write on
// others, which a concurrent writer can race.
//
// Extraction streams: each file is written while it is decompressed, and
// the limits stop the walk at the first byte past one. When the walk fails
// part-way, Extract deletes the objects it created (with a context that
// ignores ctx's cancellation) and returns the error, so a bomb or an
// unsafe entry found late leaves nothing new behind. An object it
// overwrote under Overwrite is not deleted, and keeps the archive's
// bytes. A failed cleanup is joined to the error.
func Extract(ctx context.Context, disk *filesystem.Disk, prefix string, r io.ReaderAt, size int64, opts ExtractOptions) ([]Entry, error) {
	dir, err := filesystem.CleanDir(prefix)
	if err != nil {
		return nil, err
	}
	atomic := disk.Capabilities().ConditionalWrite
	var (
		stored  []Entry
		created []string
	)
	err = walkArchive(ctx, r, size, opts.Limits, func(ctx context.Context, e Entry, content io.Reader) error {
		if e.Depth > 1 || e.IsDir || (opts.Skip != nil && opts.Skip(e)) {
			return nil
		}
		key := filesystem.JoinKey(dir, e.Name)
		existed := false
		if opts.Overwrite || !atomic {
			_, err := disk.Stat(ctx, key)
			switch {
			case err == nil:
				existed = true
			case filesystem.CodeOf(err) != filesystem.CodeNotFound:
				return err
			}
			if existed && !opts.Overwrite {
				return filesystem.Errorf(filesystem.CodeExists, "archive: %q already exists", key)
			}
		}
		// A gzip file declares no size: its Size is 0, an unknown hint.
		// The disk wraps whatever stops a write; a failure of the archive
		// (a broken stream) is told apart from one of the disk (quota,
		// outage) by remembering the entry's own read error.
		src := &readErr{r: content}
		written, err := disk.Write(ctx, key, src, filesystem.WriteOptions{SizeHint: e.Size, IfNotExists: !opts.Overwrite && atomic})
		if src.err != nil {
			return classify(src.err)
		}
		if err != nil {
			return err
		}
		if !existed {
			created = append(created, key)
		}
		e.Size = written.Size
		stored = append(stored, e)
		return nil
	})
	if err != nil {
		if len(created) > 0 {
			if derr := disk.DeleteKeys(context.WithoutCancel(ctx), created); derr != nil {
				err = errors.Join(err, derr)
			}
		}
		return nil, err
	}
	return stored, nil
}

// readErr passes r through and remembers the first error other than
// io.EOF it returned, so a caller can tell the source's failure from that
// of whatever consumed it.
type readErr struct {
	r   io.Reader
	err error
}

// Read implements io.Reader.
func (r *readErr) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && r.err == nil {
		r.err = err
	}
	return n, err
}
