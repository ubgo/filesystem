package filesystem

import "context"

// dirProbeLimit is the page size of the portable DirExists: one entry is
// enough to show a directory has something in it.
const dirProbeLimit = 1

// MakeDir creates the directory dir (and any missing parents), succeeding
// when it already exists. dir is cleaned like a key; "" (the root) always
// exists, so it does nothing.
//
// With the Dirs capability (local disk, and backends with real
// directories) the backend creates it: an empty directory DirExists
// reports until it is deleted. An object store has no directories, only
// keys that share a prefix, so without the capability MakeDir checks dir
// and does nothing else: no marker object is written, the directory
// appears when a key is written under it, and DirExists reports false
// until then. That is deliberate: a marker would be an object that List,
// DeletePrefix, quotas, mirrors and the records layer would all see, and
// that other tools reading the bucket would take for a file. Code that
// must see an empty directory checks Capabilities().Dirs.
//
// Either way the call runs as one OpMakeDir, a mutation, so a read-only
// disk refuses it on every backend.
func (d *Disk) MakeDir(ctx context.Context, dir string) error {
	clean, err := d.cleanDir(dir)
	if err != nil {
		return annotate(err, OpMakeDir, d.name, dir)
	}
	op := &Op{Kind: OpMakeDir, Key: clean, Mutating: true, Replayable: true}
	return d.do(ctx, op, func(ctx context.Context) error {
		if clean == "" || !d.caps.Dirs {
			return nil
		}
		return d.driver.(DirMaker).MakeDir(ctx, clean)
	})
}

// DirExists reports whether the directory dir exists. dir is cleaned like
// a key; the root always exists. An object at dir's key is not a
// directory.
//
// With the Dirs capability the backend answers, so an empty directory
// made by MakeDir counts. Without it a directory exists exactly while a
// key is under it, and DirExists lists dir with a limit of one entry (one
// OpList).
func (d *Disk) DirExists(ctx context.Context, dir string) (bool, error) {
	clean, err := d.cleanDir(dir)
	if err != nil {
		return false, annotate(err, OpDirExists, d.name, dir)
	}
	if clean == "" {
		return true, nil
	}
	if !d.caps.Dirs {
		page, err := d.ListPage(ctx, clean, ListOptions{Limit: dirProbeLimit})
		return len(page.Entries) > 0, err
	}
	maker := d.driver.(DirMaker)
	var exists bool
	op := &Op{Kind: OpDirExists, Key: clean, Replayable: true}
	err = d.do(ctx, op, func(ctx context.Context) error {
		ok, err := maker.DirExists(ctx, clean)
		if err == nil {
			exists = ok
		}
		return err
	})
	return exists, err
}
