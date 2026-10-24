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

// save stores an editor's new text only if the document is still the
// version the editor loaded: records.IfUnchanged with the digest it read.
// A save made in between makes it records.ErrChanged (conflict), and
// nothing changes; the editor reloads and merges.
// ds:def id=save-v4n8c2wq
func save(ctx context.Context, s *records.Store, loaded records.File, text string) (records.File, error) {
	return s.Replace(ctx, tenant, loaded.ID, strings.NewReader(text), "", textPolicy, records.IfUnchanged(loaded.SHA256))
}

// trashSeen deletes the document only if it is still the version the
// user looked at: the same option guards Trash, Restore, SetVisibility
// and Purge.
// ds:def id=trashseen-k7t3m9hp
func trashSeen(ctx context.Context, s *records.Store, seen records.File) error {
	return s.Trash(ctx, tenant, seen.ID, records.IfUnchanged(seen.SHA256))
}

// unchangedDemo has two editors load the same version and save in turn,
// then a delete of a stale version refused.
func unchangedDemo(ctx context.Context, w io.Writer, db *sql.DB) error {
	disk, err := newDisk(privateDisk)
	if err != nil {
		return err
	}
	s, err := newStore(ctx, db, []*filesystem.Disk{disk})
	if err != nil {
		return err
	}
	doc, err := s.Upload(ctx, tenant, privateDisk, strings.NewReader("draft"), "plan.txt", textPolicy)
	if err != nil {
		return err
	}
	alice, bob := doc, doc // both editors loaded the same version
	alice, err = save(ctx, s, alice, "alice's edit")
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "ifunchanged: alice saves: ok")
	_, err = save(ctx, s, bob, "bob's edit")
	fmt.Fprintf(w, "ifunchanged: bob saves the stale version: %s (%t)\n", filesystem.CodeOf(err), errors.Is(err, records.ErrChanged))
	latest, err := s.File(ctx, tenant, doc.ID)
	if err != nil {
		return err
	}
	if _, err := save(ctx, s, latest, "bob's edit, merged"); err != nil {
		return err
	}
	fmt.Fprintln(w, "ifunchanged: bob reloads and saves: ok")
	err = trashSeen(ctx, s, alice)
	fmt.Fprintf(w, "ifunchanged: trash alice's stale view: %s\n", filesystem.CodeOf(err))
	return nil
}
