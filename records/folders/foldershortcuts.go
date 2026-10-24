package folders

import (
	"context"
	"database/sql"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// TableNameFolderShortcuts holds one row per folder shortcut
// (WithFolderShortcuts): folder TargetID shown inside folder FolderID too.
// It is a table of its own, not a kind of fs_folder_shortcuts, so the file
// shortcut queries (Linked, ShortcutsOf, a purge's cleanup) never meet a
// folder ID where they expect a file's.
const TableNameFolderShortcuts = "fs_folder_folder_shortcuts"

// ColTargetID is fs_folder_folder_shortcuts' column of the folder shown;
// ColFolderID there is the folder it is shown in.
const ColTargetID = "target_id"

// FolderShortcuts and its typed columns, keyed by (scope, folder, target).
var (
	FolderShortcuts         = sqlb.Table{Name: TableNameFolderShortcuts, Alias: "ffs"}
	FolderShortcutScope     = sqlb.Col[string](FolderShortcuts, ColScope)
	FolderShortcutFolderID  = sqlb.Col[string](FolderShortcuts, ColFolderID)
	FolderShortcutTargetID  = sqlb.Col[string](FolderShortcuts, ColTargetID)
	FolderShortcutCreatedAt = sqlb.Col[time.Time](FolderShortcuts, ColCreatedAt)
)

// Event kinds of folder shortcuts: Subject is the folder shown (the
// target); Detail has DetailFolder, the folder it is shown in.
const (
	EventFolderShortcutAdded   records.EventKind = PluginName + ".folder_shortcut_added"
	EventFolderShortcutRemoved records.EventKind = PluginName + ".folder_shortcut_removed"
)

// WithFolderShortcuts lets a folder appear inside folders other than its
// parent (AddFolderShortcut, RemoveFolderShortcut, FolderShortcutsIn,
// FolderShortcutsOf), as Drive's shortcuts to folders do. As with file
// shortcuts (WithShortcuts), the parent owns the folder:
//
//   - Access: a shortcut gives no access. Ancestors and Path, which the
//     acl plugin's Hierarchy reads, follow parents only, so a grant on the
//     folder a shortcut sits in reaches nothing through it; sharing a
//     folder (Collection) shares the files whose home it is, never what
//     is linked into it.
//   - The tree is never walked through a shortcut (Path, Ancestors, Move's
//     cycle check, the depth cap), so a shortcut cannot make a cycle,
//     even one that a later Move points at an ancestor of its folder.
//   - Moving the target into a folder where it has a shortcut removes that
//     shortcut, so a folder is never shown twice in one place. Deleting a
//     folder removes the shortcuts in it and the shortcuts to it; a
//     folder holding only shortcuts counts as empty.
//
// Without it AddFolderShortcut is CodeUnsupported; shortcuts already
// stored stay listed and keep being removed with their folders.
func WithFolderShortcuts() Option { return func(p *Plugin) { p.folderShortcuts = true } }

// folderShortcutsDDL is migration 0004_folder_folder_shortcuts.
func folderShortcutsDDL(engine string) *records.DDL {
	t := records.TypesFor(engine)
	return records.NewDDL(engine).CreateTable(records.Table{
		Table: FolderShortcuts,
		Columns: []ddl.ColumnSpec{
			records.Column(FolderShortcutScope, t.Key), records.Column(FolderShortcutFolderID, t.Key),
			records.Column(FolderShortcutTargetID, t.Key), records.Column(FolderShortcutCreatedAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{FolderShortcutScope, FolderShortcutFolderID, FolderShortcutTargetID},
		Indexes: []records.Index{{Name: "fs_folder_folder_shortcuts_target", Columns: []records.IndexColumn{
			records.Key(FolderShortcutScope), records.Key(FolderShortcutTargetID),
		}}},
	})
}

// AddFolderShortcut shows folder target of scope inside folder into
// (RootID for the top level) too, besides its parent (WithFolderShortcuts;
// see it for what it means). Adding one that exists does nothing. The
// target's own parent, the target itself or a folder inside it is
// CodeInvalidArgument; an unknown folder is CodeNotFound. Without
// WithFolderShortcuts it is CodeUnsupported.
func (p *Plugin) AddFolderShortcut(ctx context.Context, s *records.Store, scope, target, into string) error {
	if !p.folderShortcuts {
		return filesystem.Errorf(filesystem.CodeUnsupported, "folders: folder shortcuts need WithFolderShortcuts")
	}
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if err := lockScope(ctx, s, scope); err != nil {
			return err
		}
		f, err := p.Folder(ctx, s, scope, target)
		if err != nil {
			return err
		}
		if f.ParentID == into {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "folders: folder %q is in %q already", f.Name, into)
		}
		path, err := p.Path(ctx, s, scope, into)
		if err != nil {
			return err
		}
		for _, a := range path {
			if a.ID == target {
				return filesystem.Errorf(filesystem.CodeInvalidArgument, "folders: cannot put a shortcut to %q inside itself", f.Name)
			}
		}
		var n int64
		q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(sqlb.Count(), &n)).From(FolderShortcuts).
			Where(sqlb.Eq(FolderShortcutScope, scope), sqlb.Eq(FolderShortcutFolderID, into), sqlb.Eq(FolderShortcutTargetID, target))
		if err := s.One(ctx, q); err != nil || n > 0 {
			return err
		}
		ins := sqlb.NewInsert(s.Dialect()).Into(FolderShortcuts).Row(sqlb.Set(FolderShortcutScope, scope), sqlb.Set(FolderShortcutFolderID, into),
			sqlb.Set(FolderShortcutTargetID, target), sqlb.Set(FolderShortcutCreatedAt, p.clock(s)))
		if _, err := s.Exec(ctx, ins); err != nil {
			return err
		}
		return s.Emit(ctx, p, records.Event{Kind: EventFolderShortcutAdded, Scope: scope, Subject: target, Detail: map[string]string{DetailFolder: into}})
	})
}

// RemoveFolderShortcut takes the shortcut to target out of folder into;
// removing one that does not exist does nothing. It works with or without
// WithFolderShortcuts, so shortcuts can always be cleared.
func (p *Plugin) RemoveFolderShortcut(ctx context.Context, s *records.Store, scope, target, into string) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		return p.removeFolderShortcuts(ctx, s, scope, sqlb.Eq(FolderShortcutFolderID, into), sqlb.Eq(FolderShortcutTargetID, target))
	})
}

// FolderShortcutsIn returns the folders shown in folder into through a
// shortcut, by name: a folder view lists them beside Children.
func (p *Plugin) FolderShortcutsIn(ctx context.Context, s *records.Store, scope, into string) ([]Folder, error) {
	var f Folder
	var out []Folder
	linked := sqlb.NewSelect(s.Dialect()).Exprs(FolderShortcutTargetID).From(FolderShortcuts).
		Where(sqlb.Eq(FolderShortcutScope, scope), sqlb.Eq(FolderShortcutFolderID, into))
	q := sqlb.NewSelect(s.Dialect()).Columns(bindings(&f)...).From(Folders).
		Where(sqlb.Eq(Scope, scope), sqlb.InSubquery(ID, linked)).Order(sqlb.OrderBy(Name, sqlb.SortAsc), sqlb.OrderBy(ID, sqlb.SortAsc))
	if err := s.Each(ctx, q, func() error { out = append(out, f); return nil }); err != nil {
		return nil, err
	}
	return out, nil
}

// FolderShortcutsOf returns the folders target has a shortcut in, by
// folder ID; its parent is not among them.
func (p *Plugin) FolderShortcutsOf(ctx context.Context, s *records.Store, scope, target string) ([]string, error) {
	var folder string
	var out []string
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(FolderShortcutFolderID, &folder)).From(FolderShortcuts).
		Where(sqlb.Eq(FolderShortcutScope, scope), sqlb.Eq(FolderShortcutTargetID, target)).Order(sqlb.OrderBy(FolderShortcutFolderID, sqlb.SortAsc))
	if err := s.Each(ctx, q, func() error { out = append(out, folder); return nil }); err != nil {
		return nil, err
	}
	return out, nil
}

// folderShortcut is one row of FolderShortcuts, for the events of a
// removal.
type folderShortcut struct {
	folder, target string
}

// removeFolderShortcuts deletes the folder shortcuts of scope matching
// where (all of them), with an EventFolderShortcutRemoved for each, inside
// the caller's transaction.
func (p *Plugin) removeFolderShortcuts(ctx context.Context, s *records.Store, scope string, where ...sqlb.Predicate) error {
	where = append([]sqlb.Predicate{sqlb.Eq(FolderShortcutScope, scope)}, where...)
	var row folderShortcut
	var gone []folderShortcut
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(FolderShortcutFolderID, &row.folder), sqlb.Bind(FolderShortcutTargetID, &row.target)).
		From(FolderShortcuts).Where(where...).Order(sqlb.OrderBy(FolderShortcutTargetID, sqlb.SortAsc), sqlb.OrderBy(FolderShortcutFolderID, sqlb.SortAsc))
	if err := s.Each(ctx, q, func() error { gone = append(gone, row); return nil }); err != nil || len(gone) == 0 {
		return err
	}
	if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(FolderShortcuts).Where(where...)); err != nil {
		return err
	}
	for _, g := range gone {
		if err := s.Emit(ctx, p, records.Event{Kind: EventFolderShortcutRemoved, Scope: scope, Subject: g.target, Detail: map[string]string{DetailFolder: g.folder}}); err != nil {
			return err
		}
	}
	return nil
}
