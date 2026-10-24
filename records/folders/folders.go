// Package folders is a records plugin for virtual folders: a tree of named
// folders per scope, and a place in it for every file. The "My Drive"
// feature of drives, document managers and media libraries.
//
// Folders exist only in the database. Moving a file between folders, or a
// folder with everything in it, changes one row and never touches stored
// objects, so it is instant on every backend. A file that was never put
// in a folder is in the root (RootID).
//
// A file has one home folder. WithShortcuts lets it also appear in other
// folders as a shortcut (Drive's "add shortcut"): a link that the home
// folder keeps owning. Moving, trashing or deleting a file goes by its
// home; access (the acl plugin's Hierarchy) goes by its home too, so a
// shortcut gives nobody access to the file (see WithShortcuts).
//
// Names are unique among siblings, compared byte for byte on every engine;
// WithNameKey adds a looser comparison (case-insensitive, as Windows and
// macOS file systems compare).
// Changes to the shape of a scope's tree (Create, Move, Delete, Put) take
// that scope's row in fs_folder_locks first, so they run one at a time
// per scope: two racing moves (A into B while B moves into A) cannot make
// a cycle, because the second one waits for the first and then sees it.
// On MySQL, whose REPEATABLE READ reads from the snapshot of a
// transaction's first read, the lock must be the transaction's first
// statement: called inside a Store.Tx of the application's own, these
// methods must come before any other read in it, or they check against
// that older snapshot. Every walk up the tree is still capped at
// MaxDepth, so a cycle written behind the plugin's back is an error,
// never a hang.
package folders

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
	"github.com/ubgo/sqlb/mysql"
	"github.com/ubgo/sqlb/pg"
	"github.com/ubgo/sqlb/sqlite"
)

// PluginName is the plugin's name in the migration ledger.
const PluginName = "folders"

// Tables the plugin owns.
const (
	TableNameFolders = "fs_folders"
	TableNamePlaces  = "fs_folder_files"
	// TableNameLocks holds one row per scope whose tree changed: the row
	// a change of shape locks (see lockScope).
	TableNameLocks = "fs_folder_locks"
	// TableNameShortcuts holds one row per shortcut (WithShortcuts): a
	// file shown in a folder other than its home.
	TableNameShortcuts = "fs_folder_shortcuts"
)

// Column names.
const (
	ColID        = "id"
	ColScope     = "scope"
	ColParentID  = "parent_id"
	ColName      = "name"
	ColCreatedAt = "created_at"
	ColFileID    = "file_id"
	ColFolderID  = "folder_id"
	// ColVersion counts the changes of shape of a scope's tree; bumping it
	// is what takes the lock.
	ColVersion = "version"
)

// Tables and typed columns.
var (
	Folders   = sqlb.Table{Name: TableNameFolders, Alias: "fd"}
	ID        = sqlb.Col[string](Folders, ColID)
	Scope     = sqlb.Col[string](Folders, ColScope)
	ParentID  = sqlb.Col[string](Folders, ColParentID)
	Name      = sqlb.Col[string](Folders, ColName)
	CreatedAt = sqlb.Col[time.Time](Folders, ColCreatedAt)

	// Places holds the folder of every file that is not in the root.
	Places        = sqlb.Table{Name: TableNamePlaces, Alias: "fp"}
	PlaceScope    = sqlb.Col[string](Places, ColScope)
	PlaceFileID   = sqlb.Col[string](Places, ColFileID)
	PlaceFolderID = sqlb.Col[string](Places, ColFolderID)

	// Locks holds the per-scope lock rows (see lockScope). It has no
	// alias: the upsert that takes the lock names it in DO UPDATE, and an
	// INSERT takes no table alias on every engine.
	Locks       = sqlb.Table{Name: TableNameLocks}
	LockScope   = sqlb.Col[string](Locks, ColScope)
	LockVersion = sqlb.Col[int64](Locks, ColVersion)

	// Shortcuts holds the shortcuts, keyed by (scope, folder, file).
	Shortcuts         = sqlb.Table{Name: TableNameShortcuts, Alias: "fsc"}
	ShortcutScope     = sqlb.Col[string](Shortcuts, ColScope)
	ShortcutFolderID  = sqlb.Col[string](Shortcuts, ColFolderID)
	ShortcutFileID    = sqlb.Col[string](Shortcuts, ColFileID)
	ShortcutCreatedAt = sqlb.Col[time.Time](Shortcuts, ColCreatedAt)
)

// Limits.
const (
	// RootID is the root folder of every scope. It has no row: parent_id
	// is RootID for a top-level folder (never NULL, so the uniqueness of
	// sibling names holds on every engine), and a file without a place is
	// in the root.
	RootID = ""
	// IDPrefix starts every folder ID.
	IDPrefix = "fld_"
	// MaxNameChars is the longest folder name, the width of its column.
	MaxNameChars = 255
	// MaxDepth is the deepest a folder may be (a top-level folder is at
	// depth 1), and the cap of every walk up the tree.
	MaxDepth = 32
)

// Event kinds the plugin emits through records.Store.Emit, inside the
// transaction of each change, so an audit log or a webhook sees folder
// changes the way it sees the Store's. A call that changes nothing (a
// rename to the current name, a move to where it already is) emits
// nothing.
const (
	// EventCreated: Subject is the folder; Detail has DetailParent and
	// DetailName.
	EventCreated records.EventKind = PluginName + ".created"
	// EventRenamed: Subject is the folder; Detail has DetailName (new) and
	// DetailFrom (old name).
	EventRenamed records.EventKind = PluginName + ".renamed"
	// EventMoved: Subject is the folder; Detail has DetailParent (new) and
	// DetailFrom (old parent).
	EventMoved records.EventKind = PluginName + ".moved"
	// EventDeleted: Subject is the folder; Detail has DetailParent and
	// DetailName.
	EventDeleted records.EventKind = PluginName + ".deleted"
	// EventFilePut is a file put in a folder (Put): FileID is the file;
	// Detail has DetailFolder (new) and DetailFrom (old folder). RootID is
	// "".
	EventFilePut records.EventKind = PluginName + ".file_put"
	// EventShortcutAdded and EventShortcutRemoved are a shortcut made or
	// taken away (AddShortcut, RemoveShortcut, and a Put or Delete that
	// removes one): FileID is the file; Detail has DetailFolder.
	EventShortcutAdded   records.EventKind = PluginName + ".shortcut_added"
	EventShortcutRemoved records.EventKind = PluginName + ".shortcut_removed"
)

// EventKindValues lists every event kind the plugin emits.
var EventKindValues = []records.EventKind{
	EventCreated, EventRenamed, EventMoved, EventDeleted, EventFilePut, EventShortcutAdded, EventShortcutRemoved,
	EventFolderShortcutAdded, EventFolderShortcutRemoved,
}

// Detail keys of the plugin's events.
const (
	DetailParent = "parent"
	DetailName   = "name"
	DetailFolder = "folder"
	DetailFrom   = "from"
)

// Folder is a folder as stored.
type Folder struct {
	ID        string
	Scope     string
	ParentID  string // RootID for a top-level folder
	Name      string
	CreatedAt time.Time
}

func bindings(f *Folder) []sqlb.Binding {
	return []sqlb.Binding{
		sqlb.Bind(ID, &f.ID), sqlb.Bind(Scope, &f.Scope), sqlb.Bind(ParentID, &f.ParentID),
		sqlb.Bind(Name, &f.Name), sqlb.Bind(CreatedAt, &f.CreatedAt),
	}
}

// Plugin manages folders. Construct it with New and install it with
// records.WithPlugins.
type Plugin struct {
	now func() time.Time
	// nameKey is WithNameKey's function; nil compares names byte for byte
	// (the sibling index alone).
	nameKey NameKey
	// shortcuts is WithShortcuts: AddShortcut is allowed.
	shortcuts bool
	// folderShortcuts is WithFolderShortcuts: AddFolderShortcut is
	// allowed.
	folderShortcuts bool
}

// Option configures a Plugin.
type Option func(*Plugin)

// NameKey maps a folder name to the key two siblings may not share: two
// names with the same key clash. It must be pure (the same name, the same
// key, always): it is asked again on every later change of the tree.
type NameKey func(name string) string

// FoldCase is a NameKey that compares names case-insensitively, by
// Unicode lower case, as Windows and macOS file systems do: "Docs" and
// "DOCS" clash. It does not normalise Unicode forms; wrap it with a
// normaliser (golang.org/x/text/unicode/norm) where precomposed and
// decomposed names must clash too.
func FoldCase(name string) string { return strings.ToLower(name) }

// WithNameKey refuses (CodeExists) a folder created, renamed or moved
// next to a sibling whose name has the same key under fn, on top of the
// byte-for-byte uniqueness the sibling index keeps. Names are still
// stored and shown as given. With it, Rename takes the scope's tree lock
// too (see lockScope), so two racing renames cannot both pass the check.
// Folders that already clash when it is turned on are left alone; only
// later changes are checked.
func WithNameKey(fn NameKey) Option { return func(p *Plugin) { p.nameKey = fn } }

// WithShortcuts lets a file appear in folders other than its home
// (AddShortcut, RemoveShortcut, ShortcutsOf, Linked), as Drive's
// shortcuts do. What each operation means with them:
//
//   - The home folder owns the file. Put moves the home and leaves the
//     shortcuts; putting a file home into a folder where it has a
//     shortcut removes that shortcut, so a file is never shown twice in
//     one folder. Files and FolderFacet list and count homes only; Linked
//     lists a folder's shortcuts.
//   - Trashing a file hides it from Linked as from Files (the trash
//     filter applies); restoring it shows it in both again. Purging it
//     removes its shortcuts.
//   - Deleting a folder removes the shortcuts in it, never their files;
//     a folder holding only shortcuts counts as empty. Moving a folder
//     moves its shortcuts with it.
//   - Access: a shortcut gives no access. FolderOf, which the acl
//     plugin's Hierarchy reads, answers the home folder only, so a grant
//     on a folder reaches the files whose home it is, not the files
//     linked into it; give a grant on the file for those. Sharing a folder
//     (Collection) shares its homes only.
//
// Without it AddShortcut is CodeUnsupported; shortcuts already stored
// stay listed and keep being removed with their file or folder.
func WithShortcuts() Option { return func(p *Plugin) { p.shortcuts = true } }

// WithClock gives this plugin its own clock, for tests. Without it the
// plugin reads records.Store.Now, so records.WithClock rules the plugin
// and the Store alike; times are kept in UTC at microsecond precision
// either way.
func WithClock(now func() time.Time) Option { return func(p *Plugin) { p.now = now } }

// New returns a Plugin.
func New(opts ...Option) *Plugin {
	p := &Plugin{}
	for _, o := range opts {
		o(p)
	}
	return p
}

// clock is WithClock's time when set, else the Store's (records.WithClock),
// in UTC at microsecond precision, the precision every engine keeps.
func (p *Plugin) clock(s *records.Store) time.Time {
	if p.now == nil {
		return s.Now()
	}
	return p.now().UTC().Truncate(time.Microsecond)
}

// Name implements records.Plugin.
func (p *Plugin) Name() string { return PluginName }

// Migrations implements records.Migrator.
func (p *Plugin) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	tables, err1 := records.NewDDL(engine).CreateTable(records.Table{
		Table: Folders,
		Columns: []ddl.ColumnSpec{
			records.Column(ID, t.Key), records.Column(Scope, t.Key), records.Column(ParentID, t.Key),
			records.Column(Name, t.Text), records.Column(CreatedAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{ID},
		Indexes: []records.Index{{Name: "fs_folders_sibling", Unique: true, Columns: []records.IndexColumn{
			records.Key(Scope), records.Key(ParentID), records.Key(Name),
		}}},
	}).CreateTable(records.Table{
		Table:      Places,
		Columns:    []ddl.ColumnSpec{records.Column(PlaceFileID, t.Key), records.Column(PlaceScope, t.Key), records.Column(PlaceFolderID, t.Key)},
		PrimaryKey: []ddl.ColumnRef{PlaceFileID},
		Indexes:    []records.Index{{Name: "fs_folder_files_folder", Columns: []records.IndexColumn{records.Key(PlaceScope), records.Key(PlaceFolderID)}}},
	}).Statements()
	locks, err2 := records.NewDDL(engine).CreateTable(records.Table{
		Table:      Locks,
		Columns:    []ddl.ColumnSpec{records.Column(LockScope, t.Key), records.Column(LockVersion, t.Int)},
		PrimaryKey: []ddl.ColumnRef{LockScope},
	}).Statements()
	shortcuts, err3 := records.NewDDL(engine).CreateTable(records.Table{
		Table: Shortcuts,
		Columns: []ddl.ColumnSpec{
			records.Column(ShortcutScope, t.Key), records.Column(ShortcutFolderID, t.Key), records.Column(ShortcutFileID, t.Key),
			records.Column(ShortcutCreatedAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{ShortcutScope, ShortcutFolderID, ShortcutFileID},
		Indexes:    []records.Index{{Name: "fs_folder_shortcuts_file", Columns: []records.IndexColumn{records.Key(ShortcutScope), records.Key(ShortcutFileID)}}},
	}).Statements()
	folderShortcuts, err4 := folderShortcutsDDL(engine).Statements()
	return []records.Migration{
		{Owner: PluginName, Name: "0001_folders", Statements: tables},
		{Owner: PluginName, Name: "0002_folder_locks", Statements: locks},
		{Owner: PluginName, Name: "0003_folder_shortcuts", Statements: shortcuts},
		{Owner: PluginName, Name: "0004_folder_folder_shortcuts", Statements: folderShortcuts},
	}, errors.Join(err1, err2, err3, err4)
}

// lockScope takes the scope's row in fs_folder_locks for the rest of the
// transaction in ctx, creating it on the scope's first change, so the
// changes of shape of one scope's tree run one at a time. It must be the
// change's first statement: on MySQL (REPEATABLE READ) a transaction reads
// from the snapshot of its first read, and only reads made after the lock
// see the change that held it. It is one upsert, because an UPDATE that
// matches nothing followed by an INSERT would make two first changes
// deadlock on MySQL's gap locks, or fail a Postgres transaction on the
// losing INSERT. SQLite has one writer at a time and needs no row lock,
// but runs the same statement.
func lockScope(ctx context.Context, s *records.Store, scope string) error {
	row := []sqlb.Assignment{sqlb.Set(LockScope, scope), sqlb.Set(LockVersion, int64(1))}
	bump := sqlb.SetExpr(LockVersion, sqlb.Add(LockVersion, sqlb.Param[int64](1)))
	var st records.Statement
	switch sqlb.DialectName(s.Dialect()) {
	case records.EngineMySQL:
		st = mysql.Insert(Locks).Row(row...).Clause(mysql.OnDuplicateKeyUpdate(bump))
	case records.EnginePostgres:
		st = pg.Insert(Locks).Row(row...).Clause(pg.OnConflict(LockScope).DoUpdate(bump).Clause())
	default:
		st = sqlite.Insert(Locks).Row(row...).Clause(sqlite.OnConflict(LockScope).DoUpdate(bump).Clause())
	}
	_, err := s.Exec(ctx, st)
	return err
}

// BeforeCreate implements records.FileHooks.
func (p *Plugin) BeforeCreate(context.Context, *records.Store, *records.File) error { return nil }

// AfterCreate implements records.FileHooks.
func (p *Plugin) AfterCreate(context.Context, *records.Store, records.File) error { return nil }

// BeforePurge implements records.FileHooks: a purged file leaves its
// folder, and its shortcuts go.
func (p *Plugin) BeforePurge(ctx context.Context, s *records.Store, f records.File) error {
	if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Places).Where(sqlb.Eq(PlaceScope, f.Scope), sqlb.Eq(PlaceFileID, f.ID))); err != nil {
		return err
	}
	_, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Shortcuts).Where(sqlb.Eq(ShortcutScope, f.Scope), sqlb.Eq(ShortcutFileID, f.ID)))
	return err
}

// PurgeScope implements records.ScopePurger: once Store.PurgeScope has
// purged every file of scope, the scope's folder tree, the places left in
// it and its lock row go too.
func (p *Plugin) PurgeScope(ctx context.Context, s *records.Store, scope string) error {
	for _, del := range []records.Statement{
		sqlb.NewDelete(s.Dialect()).From(Places).Where(sqlb.Eq(PlaceScope, scope)),
		sqlb.NewDelete(s.Dialect()).From(Shortcuts).Where(sqlb.Eq(ShortcutScope, scope)),
		sqlb.NewDelete(s.Dialect()).From(FolderShortcuts).Where(sqlb.Eq(FolderShortcutScope, scope)),
		sqlb.NewDelete(s.Dialect()).From(Folders).Where(sqlb.Eq(Scope, scope)),
		sqlb.NewDelete(s.Dialect()).From(Locks).Where(sqlb.Eq(LockScope, scope)),
	} {
		if _, err := s.Exec(ctx, del); err != nil {
			return err
		}
	}
	return nil
}

// CheckName reports whether name can name a folder: 1 to MaxNameChars
// characters of valid UTF-8, no "/", no control characters, no leading or
// trailing spaces, and not "." or "..". These are the names that survive
// a download as a zip archive or a sync to a real file system.
func CheckName(name string) error {
	switch {
	case name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > MaxNameChars:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "folders: a name must be 1 to %d characters of valid UTF-8", MaxNameChars)
	case name == "." || name == "..":
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "folders: %q cannot name a folder", name)
	case strings.ContainsRune(name, '/') || strings.IndexFunc(name, unicode.IsControl) >= 0:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "folders: a name cannot contain \"/\" or control characters")
	case strings.TrimSpace(name) != name:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "folders: a name cannot start or end with spaces")
	}
	return nil
}

// checkSiblings refuses, with WithNameKey, name in parent when a sibling
// other than self has a name with the same key. It runs after lockScope,
// so no change of the tree lands between the check and the write.
func (p *Plugin) checkSiblings(ctx context.Context, s *records.Store, scope, parent, name, self string) error {
	if p.nameKey == nil {
		return nil
	}
	siblings, err := p.Children(ctx, s, scope, parent)
	if err != nil {
		return err
	}
	key := p.nameKey(name)
	for _, sib := range siblings {
		if sib.ID != self && p.nameKey(sib.Name) == key {
			return filesystem.Errorf(filesystem.CodeExists, "folders: %q clashes with %q, already there", name, sib.Name)
		}
	}
	return nil
}

func noFolder(id string) error {
	return filesystem.Errorf(filesystem.CodeNotFound, "folders: no folder %q", id)
}

// nameTaken maps a violation of the sibling index to CodeExists. On
// Postgres the failed statement aborts the transaction, so the caller's
// transaction (if any) must end too.
func nameTaken(err error, name string) error {
	if records.IsUniqueViolation(err) {
		return filesystem.Errorf(filesystem.CodeExists, "folders: a folder named %q is already there", name)
	}
	return err
}

// Folder returns one folder of scope, or CodeNotFound.
func (p *Plugin) Folder(ctx context.Context, s *records.Store, scope, id string) (Folder, error) {
	var f Folder
	q := sqlb.NewSelect(s.Dialect()).Columns(bindings(&f)...).From(Folders).Where(sqlb.Eq(Scope, scope), sqlb.Eq(ID, id))
	if err := s.One(ctx, q); err != nil {
		if errors.Is(err, sqlb.ErrNoRows) {
			return Folder{}, noFolder(id)
		}
		return Folder{}, err
	}
	return f, nil
}

// Path returns the folders from the top level down to id, for
// breadcrumbs. The root's path is empty.
func (p *Plugin) Path(ctx context.Context, s *records.Store, scope, id string) ([]Folder, error) {
	var path []Folder
	for id != RootID {
		if len(path) == MaxDepth {
			return nil, filesystem.Errorf(filesystem.CodeUnknown, "folders: %q is deeper than %d, or in a cycle", id, MaxDepth)
		}
		f, err := p.Folder(ctx, s, scope, id)
		if err != nil {
			return nil, err
		}
		path = append(path, f)
		id = f.ParentID
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path, nil
}

// Create makes a folder named name in parent (RootID for the top level).
// A sibling with the same name is CodeExists.
func (p *Plugin) Create(ctx context.Context, s *records.Store, scope, parent, name string) (Folder, error) {
	if err := CheckName(name); err != nil {
		return Folder{}, err
	}
	var f Folder
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if err := lockScope(ctx, s, scope); err != nil {
			return err
		}
		path, err := p.Path(ctx, s, scope, parent)
		if err != nil {
			return err
		}
		if len(path) == MaxDepth {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "folders: folders nest at most %d deep", MaxDepth)
		}
		if err := p.checkSiblings(ctx, s, scope, parent, name, ""); err != nil {
			return err
		}
		f = Folder{ID: s.NewID(IDPrefix), Scope: scope, ParentID: parent, Name: name, CreatedAt: p.clock(s)}
		ins := sqlb.NewInsert(s.Dialect()).Into(Folders).Row(
			sqlb.Set(ID, f.ID), sqlb.Set(Scope, f.Scope), sqlb.Set(ParentID, f.ParentID), sqlb.Set(Name, f.Name), sqlb.Set(CreatedAt, f.CreatedAt),
		)
		if _, err := s.Exec(ctx, ins); err != nil {
			return nameTaken(err, name)
		}
		return s.Emit(ctx, p, records.Event{Kind: EventCreated, Scope: scope, Subject: f.ID, Detail: map[string]string{DetailParent: parent, DetailName: name}})
	})
	if err != nil {
		return Folder{}, err
	}
	return f, nil
}

// Children returns the folders directly inside parent (RootID for the top
// level), by name.
func (p *Plugin) Children(ctx context.Context, s *records.Store, scope, parent string) ([]Folder, error) {
	var f Folder
	var out []Folder
	q := sqlb.NewSelect(s.Dialect()).Columns(bindings(&f)...).From(Folders).
		Where(sqlb.Eq(Scope, scope), sqlb.Eq(ParentID, parent)).Order(sqlb.OrderBy(Name, sqlb.SortAsc))
	if err := s.Each(ctx, q, func() error { out = append(out, f); return nil }); err != nil {
		return nil, err
	}
	return out, nil
}

// Rename renames a folder; a sibling with the new name is CodeExists.
// Renaming to the current name does nothing.
func (p *Plugin) Rename(ctx context.Context, s *records.Store, scope, id, name string) error {
	if err := CheckName(name); err != nil {
		return err
	}
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if p.nameKey != nil {
			// The sibling check needs the tree held still; without it the
			// sibling index alone decides, and needs no lock.
			if err := lockScope(ctx, s, scope); err != nil {
				return err
			}
		}
		f, err := p.Folder(ctx, s, scope, id)
		if err != nil || f.Name == name {
			return err
		}
		if err := p.checkSiblings(ctx, s, scope, f.ParentID, name, id); err != nil {
			return err
		}
		upd := sqlb.NewUpdate(s.Dialect()).Table(Folders).Set(sqlb.Set(Name, name)).Where(sqlb.Eq(Scope, scope), sqlb.Eq(ID, id))
		n, err := s.Exec(ctx, upd)
		if err != nil {
			return nameTaken(err, name)
		}
		if n == 0 {
			// Deleted between the read and the write (a racing Delete):
			// the name changes, so on every engine n is 1 for a live row.
			return noFolder(id)
		}
		return s.Emit(ctx, p, records.Event{Kind: EventRenamed, Scope: scope, Subject: id, Detail: map[string]string{DetailName: name, DetailFrom: f.Name}})
	})
}

// Move puts folder id, with everything in it, into parent (RootID for the
// top level). Moving a folder into itself or below itself, or so deep
// that its subtree would pass MaxDepth, is CodeInvalidArgument. Moving it
// where it already is does nothing.
func (p *Plugin) Move(ctx context.Context, s *records.Store, scope, id, parent string) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if err := lockScope(ctx, s, scope); err != nil {
			return err
		}
		f, err := p.Folder(ctx, s, scope, id)
		if err != nil || f.ParentID == parent {
			return err
		}
		path, err := p.Path(ctx, s, scope, parent)
		if err != nil {
			return err
		}
		for _, a := range path {
			if a.ID == id {
				return filesystem.Errorf(filesystem.CodeInvalidArgument, "folders: cannot move %q into itself", f.Name)
			}
		}
		height, err := p.height(ctx, s, scope, id)
		if err != nil {
			return err
		}
		if len(path)+height > MaxDepth {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "folders: folders nest at most %d deep", MaxDepth)
		}
		if err := p.checkSiblings(ctx, s, scope, parent, f.Name, id); err != nil {
			return err
		}
		upd := sqlb.NewUpdate(s.Dialect()).Table(Folders).Set(sqlb.Set(ParentID, parent)).Where(sqlb.Eq(Scope, scope), sqlb.Eq(ID, id))
		if _, err := s.Exec(ctx, upd); err != nil {
			return nameTaken(err, f.Name)
		}
		// Never shown twice in one folder: its shortcut there goes.
		if err := p.removeFolderShortcuts(ctx, s, scope, sqlb.Eq(FolderShortcutFolderID, parent), sqlb.Eq(FolderShortcutTargetID, id)); err != nil {
			return err
		}
		return s.Emit(ctx, p, records.Event{Kind: EventMoved, Scope: scope, Subject: id, Detail: map[string]string{DetailParent: parent, DetailFrom: f.ParentID}})
	})
}

// height is the number of levels in the subtree of id, counting id: 1 for
// a folder without subfolders. It reads one level per query.
func (p *Plugin) height(ctx context.Context, s *records.Store, scope, id string) (int, error) {
	level := []string{id}
	for height := 1; ; height++ {
		if height > MaxDepth {
			return 0, filesystem.Errorf(filesystem.CodeUnknown, "folders: the subtree of %q is deeper than %d, or a cycle", id, MaxDepth)
		}
		var child string
		var next []string
		q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(ID, &child)).From(Folders).Where(sqlb.Eq(Scope, scope), sqlb.In(ParentID, level...))
		if err := s.Each(ctx, q, func() error { next = append(next, child); return nil }); err != nil {
			return 0, err
		}
		if len(next) == 0 {
			return height, nil
		}
		level = next
	}
}

// Delete removes an empty folder. A folder holding subfolders or live
// files is CodeInvalidArgument; files of it that are in the trash move to
// the root, so restoring one never points into a deleted folder; the
// shortcuts in it go (WithShortcuts), their files staying where they are,
// and so do the folder shortcuts in it and to it (WithFolderShortcuts).
// Deleting an unknown folder is CodeNotFound.
func (p *Plugin) Delete(ctx context.Context, s *records.Store, scope, id string) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if err := lockScope(ctx, s, scope); err != nil {
			return err
		}
		f, err := p.Folder(ctx, s, scope, id)
		if err != nil {
			return err
		}
		children, err := p.Children(ctx, s, scope, id)
		if err != nil {
			return err
		}
		live, err := s.Files(ctx, records.FileQuery{Scope: scope, Limit: 1, Filters: []sqlb.Predicate{p.in(s, scope, id)}})
		if err != nil {
			return err
		}
		if len(children) > 0 || len(live.Files) > 0 {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "folders: %q is not empty", f.Name)
		}
		if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Places).Where(sqlb.Eq(PlaceScope, scope), sqlb.Eq(PlaceFolderID, id))); err != nil {
			return err
		}
		if err := p.removeShortcuts(ctx, s, scope, sqlb.Eq(ShortcutFolderID, id)); err != nil {
			return err
		}
		if err := p.removeFolderShortcuts(ctx, s, scope, sqlb.Or(sqlb.Eq(FolderShortcutFolderID, id), sqlb.Eq(FolderShortcutTargetID, id))); err != nil {
			return err
		}
		if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Folders).Where(sqlb.Eq(Scope, scope), sqlb.Eq(ID, id))); err != nil {
			return err
		}
		return s.Emit(ctx, p, records.Event{Kind: EventDeleted, Scope: scope, Subject: id, Detail: map[string]string{DetailParent: f.ParentID, DetailName: f.Name}})
	})
}

// in is the filter on records.TableFiles that keeps the files in folder.
func (p *Plugin) in(s *records.Store, scope, folder string) sqlb.Predicate {
	placed := sqlb.NewSelect(s.Dialect()).Exprs(PlaceFileID).From(Places).Where(sqlb.Eq(PlaceScope, scope))
	if folder == RootID {
		return sqlb.NotInSubquery(records.FileID, placed)
	}
	return sqlb.InSubquery(records.FileID, placed.Where(sqlb.Eq(PlaceFolderID, folder)))
}

// Files lists the files directly in folder (RootID for the root) with the
// order, pagination and trash filter of records.Store.Files; fq.Scope is
// the scope.
func (p *Plugin) Files(ctx context.Context, s *records.Store, folder string, fq records.FileQuery) (records.FilePage, error) {
	if folder != RootID {
		if _, err := p.Folder(ctx, s, fq.Scope, folder); err != nil {
			return records.FilePage{}, err
		}
	}
	fq.Filters = append(fq.Filters[:len(fq.Filters):len(fq.Filters)], p.in(s, fq.Scope, folder))
	return s.Files(ctx, fq)
}

// FolderFacet counts the files fq selects (its Scope, Trashed and Filters,
// as records.Store.Facets; Limit and Cursor must be unset) by the folder
// each is directly in: the folder facet of a media library's sidebar,
// beside the Store's content-type and visibility facets. Value is the
// folder ID, RootID ("") for files in the root. Most files first, then
// by folder ID.
func (p *Plugin) FolderFacet(ctx context.Context, s *records.Store, fq records.FileQuery) ([]records.FacetCount, error) {
	if fq.Limit != 0 || fq.Cursor != "" {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "folders: FolderFacet counts every file the query selects; Limit and Cursor do not apply")
	}
	where := []sqlb.Predicate{sqlb.Eq(records.FileScope, fq.Scope)}
	if fq.Trashed {
		where = append(where, sqlb.IsNotNull(records.FileDeletedAt))
	} else {
		where = append(where, sqlb.IsNull(records.FileDeletedAt))
	}
	where = append(where, fq.Filters...)
	var c records.FacetCount
	var out []records.FacetCount
	folder := sqlb.Coalesce(PlaceFolderID, sqlb.Param(RootID))
	n := sqlb.Count()
	bytes := sqlb.Coalesce(sqlb.Sum(records.FileSize), sqlb.Param[int64](0))
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(folder, &c.Value), sqlb.Bind(n, &c.Files), sqlb.Bind(bytes, &c.Bytes)).
		From(records.TableFiles).
		Join(sqlb.Join{Type: sqlb.JoinLeft, Table: Places, On: []sqlb.Predicate{sqlb.EqExpr(PlaceFileID, records.FileID), sqlb.EqExpr(PlaceScope, records.FileScope)}}).
		Where(where...).GroupBy(PlaceFolderID).Order(sqlb.OrderBy(n, sqlb.SortDesc), sqlb.OrderBy(folder, sqlb.SortAsc))
	if err := s.Each(ctx, q, func() error { out = append(out, c); return nil }); err != nil {
		return nil, err
	}
	return out, nil
}

// Put moves a file of scope into folder (RootID for the root). Putting
// it where it already is does nothing. A shortcut of the file in folder
// is removed, the folder now being its home (WithShortcuts).
func (p *Plugin) Put(ctx context.Context, s *records.Store, scope, fileID, folder string) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if err := lockScope(ctx, s, scope); err != nil {
			return err
		}
		from, err := p.FolderOf(ctx, s, scope, fileID)
		if err != nil || from == folder {
			return err
		}
		if folder != RootID {
			if _, err := p.Folder(ctx, s, scope, folder); err != nil {
				return err
			}
		}
		if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Places).Where(sqlb.Eq(PlaceScope, scope), sqlb.Eq(PlaceFileID, fileID))); err != nil {
			return err
		}
		if folder != RootID {
			ins := sqlb.NewInsert(s.Dialect()).Into(Places).Row(sqlb.Set(PlaceFileID, fileID), sqlb.Set(PlaceScope, scope), sqlb.Set(PlaceFolderID, folder))
			if _, err := s.Exec(ctx, ins); err != nil {
				return err
			}
			if err := p.removeShortcuts(ctx, s, scope, sqlb.Eq(ShortcutFolderID, folder), sqlb.Eq(ShortcutFileID, fileID)); err != nil {
				return err
			}
		}
		return s.Emit(ctx, p, records.Event{Kind: EventFilePut, Scope: scope, FileID: fileID, Detail: map[string]string{DetailFolder: folder, DetailFrom: from}})
	})
}

// FolderOf returns the folder a file of scope is in: RootID for the root.
// It is the file's home; its shortcuts (WithShortcuts) are not folders it
// is in, for this answer and for the acl plugin that reads it.
func (p *Plugin) FolderOf(ctx context.Context, s *records.Store, scope, fileID string) (string, error) {
	if _, err := s.File(ctx, scope, fileID); err != nil {
		return "", err
	}
	var folder string
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(PlaceFolderID, &folder)).From(Places).Where(sqlb.Eq(PlaceScope, scope), sqlb.Eq(PlaceFileID, fileID))
	if err := s.One(ctx, q); err != nil && !errors.Is(err, sqlb.ErrNoRows) {
		return "", err
	}
	return folder, nil
}
