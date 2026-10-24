package folders

import (
	"context"
	"database/sql"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// shortcut is one row of Shortcuts, for the events of a removal.
type shortcut struct {
	folder, file string
}

// AddShortcut shows a live file of scope in folder too, besides its home
// (WithShortcuts; see it for what moving, trashing and deleting mean).
// Adding one that exists does nothing. The file's home folder is
// CodeInvalidArgument (it is there already); an unknown or trashed file,
// or an unknown folder, is CodeNotFound. Without WithShortcuts it is
// CodeUnsupported.
func (p *Plugin) AddShortcut(ctx context.Context, s *records.Store, scope, fileID, folder string) error {
	if !p.shortcuts {
		return filesystem.Errorf(filesystem.CodeUnsupported, "folders: shortcuts need WithShortcuts")
	}
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		// The tree lock first, as every change of the tree: a racing
		// Delete of folder then either commits first (and the folder is
		// not found) or waits for this shortcut and removes it.
		if err := lockScope(ctx, s, scope); err != nil {
			return err
		}
		f, err := s.File(ctx, scope, fileID)
		if err != nil {
			return err
		}
		if f.DeletedAt != nil {
			return filesystem.Errorf(filesystem.CodeNotFound, "folders: file %q is in the trash", fileID)
		}
		if folder != RootID {
			if _, err := p.Folder(ctx, s, scope, folder); err != nil {
				return err
			}
		}
		home, err := p.FolderOf(ctx, s, scope, fileID)
		if err != nil {
			return err
		}
		if home == folder {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "folders: folder %q is the file's home already", folder)
		}
		var n int64
		q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(sqlb.Count(), &n)).From(Shortcuts).
			Where(sqlb.Eq(ShortcutScope, scope), sqlb.Eq(ShortcutFolderID, folder), sqlb.Eq(ShortcutFileID, fileID))
		if err := s.One(ctx, q); err != nil || n > 0 {
			return err
		}
		ins := sqlb.NewInsert(s.Dialect()).Into(Shortcuts).Row(sqlb.Set(ShortcutScope, scope), sqlb.Set(ShortcutFolderID, folder),
			sqlb.Set(ShortcutFileID, fileID), sqlb.Set(ShortcutCreatedAt, p.clock(s)))
		if _, err := s.Exec(ctx, ins); err != nil {
			return err
		}
		return s.Emit(ctx, p, records.Event{Kind: EventShortcutAdded, Scope: scope, FileID: fileID, Detail: map[string]string{DetailFolder: folder}})
	})
}

// RemoveShortcut takes a file's shortcut out of folder; removing one that
// does not exist does nothing. The file and its home are untouched. It
// works with or without WithShortcuts, so shortcuts can always be
// cleared.
func (p *Plugin) RemoveShortcut(ctx context.Context, s *records.Store, scope, fileID, folder string) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		return p.removeShortcuts(ctx, s, scope, sqlb.Eq(ShortcutFolderID, folder), sqlb.Eq(ShortcutFileID, fileID))
	})
}

// ShortcutsOf returns the folders a file of scope has a shortcut in, by
// folder ID; its home (FolderOf) is not among them.
func (p *Plugin) ShortcutsOf(ctx context.Context, s *records.Store, scope, fileID string) ([]string, error) {
	var folder string
	var out []string
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(ShortcutFolderID, &folder)).From(Shortcuts).
		Where(sqlb.Eq(ShortcutScope, scope), sqlb.Eq(ShortcutFileID, fileID)).Order(sqlb.OrderBy(ShortcutFolderID, sqlb.SortAsc))
	if err := s.Each(ctx, q, func() error { out = append(out, folder); return nil }); err != nil {
		return nil, err
	}
	return out, nil
}

// Linked lists the files with a shortcut in folder (RootID for the root),
// with the order, pagination and trash filter of records.Store.Files, as
// Files lists the files whose home it is; fq.Scope is the scope. A folder
// view shows both lists.
func (p *Plugin) Linked(ctx context.Context, s *records.Store, folder string, fq records.FileQuery) (records.FilePage, error) {
	if folder != RootID {
		if _, err := p.Folder(ctx, s, fq.Scope, folder); err != nil {
			return records.FilePage{}, err
		}
	}
	linked := sqlb.NewSelect(s.Dialect()).Exprs(ShortcutFileID).From(Shortcuts).
		Where(sqlb.Eq(ShortcutScope, fq.Scope), sqlb.Eq(ShortcutFolderID, folder))
	fq.Filters = append(fq.Filters[:len(fq.Filters):len(fq.Filters)], sqlb.InSubquery(records.FileID, linked))
	return s.Files(ctx, fq)
}

// removeShortcuts deletes the shortcuts of scope matching where, with an
// EventShortcutRemoved for each, inside the caller's transaction.
func (p *Plugin) removeShortcuts(ctx context.Context, s *records.Store, scope string, where ...sqlb.Predicate) error {
	where = append([]sqlb.Predicate{sqlb.Eq(ShortcutScope, scope)}, where...)
	var row shortcut
	var gone []shortcut
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(ShortcutFolderID, &row.folder), sqlb.Bind(ShortcutFileID, &row.file)).
		From(Shortcuts).Where(where...).Order(sqlb.OrderBy(ShortcutFileID, sqlb.SortAsc))
	if err := s.Each(ctx, q, func() error { gone = append(gone, row); return nil }); err != nil || len(gone) == 0 {
		return err
	}
	if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Shortcuts).Where(where...)); err != nil {
		return err
	}
	for _, g := range gone {
		if err := s.Emit(ctx, p, records.Event{Kind: EventShortcutRemoved, Scope: scope, FileID: g.file, Detail: map[string]string{DetailFolder: g.folder}}); err != nil {
			return err
		}
	}
	return nil
}
