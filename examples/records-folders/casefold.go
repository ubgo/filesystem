package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/folders"
)

// newCaseInsensitive is the folders plugin for a product that syncs to
// Windows or macOS: sibling names that differ only in letter case clash.
// Names are still stored and shown as typed.
// ds:def id=newcaseinsensitive-t5k2w8jr
func newCaseInsensitive() *folders.Plugin {
	return folders.New(folders.WithNameKey(folders.FoldCase))
}

// clashes creates "Docs" and tries "docs" beside it, then renames another
// folder to "DOCS" and "Docs" to its own case variant, reporting each
// outcome as "ok" or the error code.
func clashes(ctx context.Context, s *records.Store, fp *folders.Plugin) ([]string, error) {
	docs, err := fp.Create(ctx, s, tenant, folders.RootID, "Docs")
	if err != nil {
		return nil, err
	}
	reports, err := fp.Create(ctx, s, tenant, folders.RootID, "Reports")
	if err != nil {
		return nil, err
	}
	outcome := func(err error) string {
		if err != nil {
			return string(filesystem.CodeOf(err))
		}
		return "ok"
	}
	_, err = fp.Create(ctx, s, tenant, folders.RootID, "docs")
	return []string{
		outcome(err),
		outcome(fp.Rename(ctx, s, tenant, reports.ID, "DOCS")),
		outcome(fp.Rename(ctx, s, tenant, docs.ID, "DOCS")),
	}, nil
}

// caseNames runs clashes on the default plugin and on the
// case-insensitive one, each over a database of its own in dir.
func caseNames(ctx context.Context, w io.Writer, dir string) error {
	for i, c := range []struct {
		name string
		fp   *folders.Plugin
	}{{"byte for byte", folders.New()}, {"FoldCase", newCaseInsensitive()}} {
		db, err := openSQLite(filepath.Join(dir, fmt.Sprintf("case-%d.db", i)))
		if err != nil {
			return err
		}
		disks, err := newDisks()
		if err != nil {
			_ = db.Close()
			return err
		}
		s, err := newStore(ctx, db, disks, c.fp)
		if err != nil {
			_ = db.Close()
			return err
		}
		got, err := clashes(ctx, s, c.fp)
		_ = db.Close()
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%s: create docs beside Docs: %s; rename Reports to DOCS: %s; rename Docs to DOCS: %s\n", c.name, got[0], got[1], got[2])
	}
	return nil
}
