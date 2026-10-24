package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// rolledBackUpload uploads inside the application's transaction, then a
// later statement fails. The rollback removes the row, and the Store
// deletes the object it had already written, so nothing is left.
// ds:def id=rolledbackupload-v4hfbev6
func rolledBackUpload(ctx context.Context, d *demo, out io.Writer) error {
	err := d.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := d.store.Upload(ctx, scope, diskName, strings.NewReader(pdf), "invoice.pdf", policy); err != nil {
			return err
		}
		return errAppFailed
	})
	objects, rows, serr := d.state(ctx)
	if serr != nil {
		return serr
	}
	fmt.Fprintf(out, "rolled back upload: %v; objects %d, rows %d\n", errors.Is(err, errAppFailed), objects, rows)
	return nil
}

// objectFirst records a key that was never stored. A row must never point
// at missing bytes, so CreateFile checks the object first.
// ds:def id=objectfirst-e5ygz2n4
func objectFirst(ctx context.Context, d *demo, out io.Writer) error {
	_, err := d.store.CreateFile(ctx, records.NewFile{Scope: scope, Disk: diskName, Key: "uploads/never-written.pdf", Filename: "x.pdf"})
	fmt.Fprintf(out, "record a missing object: %s\n", filesystem.CodeOf(err))
	return nil
}

// purgeAfterCommit purges a file twice: in a transaction that rolls back,
// which leaves both the row and the object, and then for real, which
// deletes the row and, after the commit, the object.
// ds:def id=purgeaftercommit-cjatkcy8
func purgeAfterCommit(ctx context.Context, d *demo, out io.Writer) error {
	f, err := d.store.Upload(ctx, scope, diskName, strings.NewReader(pdf), "keep.pdf", policy)
	if err != nil {
		return err
	}
	err = d.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if err := d.store.Purge(ctx, scope, f.ID); err != nil {
			return err
		}
		return errAppFailed
	})
	objects, rows, serr := d.state(ctx)
	if serr != nil {
		return serr
	}
	fmt.Fprintf(out, "purge rolled back: %v; objects %d, rows %d\n", errors.Is(err, errAppFailed), objects, rows)
	if err := d.store.Purge(ctx, scope, f.ID); err != nil {
		return err
	}
	objects, rows, err = d.state(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "purge committed: objects %d, rows %d\n", objects, rows)
	return nil
}
