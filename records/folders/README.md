# github.com/ubgo/filesystem/records/folders

Virtual folders for the records Store: a tree of named folders per scope and a place in it for every file. Folders live only in the database, so moving a file or a whole folder changes one row and never touches stored objects. The guide is [docs/plugins/folders.md](../../docs/plugins/folders.md).

## Install

```sh
go get github.com/ubgo/filesystem/records
```

The package is in the records module, which requires only `github.com/ubgo/sqlb` and the core module.

## Wire it

<!-- ds:block id=newstore-q3g7kahh -->
**newStore** · [`examples/records-folders/main.go:72-81`](../../examples/records-folders/main.go#L72-L81)

```go
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager, fp *folders.Plugin) (*records.Store, error) {
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(fp))
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
```
<!-- /ds:block hash=4b044f -->

<!-- ds:block id=uploadinto-dszbu3j3 -->
**uploadInto** · [`examples/records-folders/main.go:119-129`](../../examples/records-folders/main.go#L119-L129)

```go
func uploadInto(ctx context.Context, s *records.Store, fp *folders.Plugin, folder, name string) (records.File, error) {
	var f records.File
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var err error
		if f, err = s.Upload(ctx, tenant, diskName, strings.NewReader("content of "+name), name, textPolicy); err != nil {
			return err
		}
		return fp.Put(ctx, s, tenant, f.ID, folder)
	})
	return f, err
}
```
<!-- /ds:block hash=938964 -->

<!-- ds:block id=listfolder-9ydubcfn -->
**listFolder** · [`examples/records-folders/main.go:134-150`](../../examples/records-folders/main.go#L134-L150)

```go
func listFolder(ctx context.Context, s *records.Store, fp *folders.Plugin, folder string) ([]string, int, error) {
	var names []string
	q := records.FileQuery{Scope: tenant, Limit: folderPage}
	for pages := 1; ; pages++ {
		page, err := fp.Files(ctx, s, folder, q)
		if err != nil {
			return nil, pages, err
		}
		for _, f := range page.Files {
			names = append(names, f.Filename)
		}
		if page.NextCursor == "" {
			return names, pages, nil
		}
		q.Cursor = page.NextCursor
	}
}
```
<!-- /ds:block hash=04b3a8 -->

`Migrate` creates `fs_folders` and `fs_folder_files`.

The plugin depends on no other plugin and serves no route. Every method takes the Store explicitly and joins the transaction in `ctx` when there is one.

## Rules

- The root of every scope is `RootID` (`""`). It has no row; a top-level folder's parent is `RootID`, and a file without a place is in the root.
- Names are unique among siblings, byte for byte on every engine. `CheckName` defines a valid name: 1 to `MaxNameChars` (255) characters of valid UTF-8, no `/`, no control characters, no leading or trailing spaces, not `.` or `..`.
- Folders nest at most `MaxDepth` (32) deep. `Create`, `Move`, `Delete` and `Put` first lock the scope's row in `fs_folder_locks`, so changes to one scope's tree run one at a time and two racing moves cannot make a cycle; on MySQL, call them before any other read in a transaction of your own (REPEATABLE READ reads from the first read's snapshot). Every walk is still capped at `MaxDepth`, so a cycle written behind the plugin's back is an error, never a hang.
- `Rename` to the current name, and `Move` or `Put` to where it already is, do nothing and emit no event.
- `records.Store.PurgeScope` deletes the scope's folders, places and lock row (`ScopePurger`).
- `Delete` removes only an empty folder (no subfolders, no live files); its trashed files move to the root.
- A purged file leaves its folder (`BeforePurge`). Trash and restore keep the file's place.

## Events

Every change emits a `records.Event` through `Store.Emit` inside its transaction (a call that changes nothing emits nothing; a hook's error rolls the change back). `EventKindValues` lists them.

| Kind | Method | Fields |
|---|---|---|
| `EventCreated` (`folders.created`) | `Create` | `Subject` (folder); `Detail` `DetailParent`, `DetailName` |
| `EventRenamed` (`folders.renamed`) | `Rename` | `Subject`; `Detail` `DetailName` (new), `DetailFrom` (old) |
| `EventMoved` (`folders.moved`) | `Move` | `Subject`; `Detail` `DetailParent` (new), `DetailFrom` (old parent) |
| `EventDeleted` (`folders.deleted`) | `Delete` | `Subject`; `Detail` `DetailParent`, `DetailName` |
| `EventFilePut` (`folders.file_put`) | `Put` | `FileID`; `Detail` `DetailFolder` (new), `DetailFrom` (old); the root is `""` |
| `EventShortcutAdded` (`folders.shortcut_added`), `EventShortcutRemoved` (`folders.shortcut_removed`) | `AddShortcut`; `RemoveShortcut`, and a `Put` or `Delete` that removes one | `FileID`; `Detail` `DetailFolder` |
| `EventFolderShortcutAdded` (`folders.folder_shortcut_added`), `EventFolderShortcutRemoved` (`folders.folder_shortcut_removed`) | `AddFolderShortcut`; `RemoveFolderShortcut`, and a `Move` or `Delete` that removes one | `Subject` (the folder shown); `Detail` `DetailFolder` (where) |

## Sharing and permissions

`(*Plugin).Collection()` is the folders as a `share` collection and receiver (`share.WithCollections(p.Collection())`, kind `folders.KindFolder`): a link shares a folder's own files, and a request link puts uploads in it. `(*Plugin).Ancestors(ctx, s, scope, id)` returns a folder and the folders above it, nearest first; with `FolderOf` it makes the plugin an `acl.Hierarchy`, so grants on a folder reach its files and subfolders.

## Options

| Option | What it does | Default |
|---|---|---|
| `folders.WithNameKey(fn)` | Refuses (`exists`) a folder created, renamed or moved beside a sibling whose name has the same key under `fn`; `folders.FoldCase` makes names case-insensitive as Windows and macOS compare them. Names are stored as given; `Rename` then takes the scope's tree lock | byte-for-byte names (the sibling index) |
| `folders.WithShortcuts()` | A file may also appear in other folders than its home (`AddShortcut`, Drive's shortcuts). The home owns it: `Put` moves the home and keeps the shortcuts (one into the new home is removed), `Files` and `FolderFacet` list homes, `Linked` lists shortcuts; trash hides it from both lists and restore brings it back; a purge removes its shortcuts; deleting a folder removes the shortcuts in it, never their files. A shortcut gives no access: `FolderOf` (the acl `Hierarchy`) answers the home only, and a folder share shares homes only | one folder per file; `AddShortcut` is `unsupported` |
| `folders.WithFolderShortcuts()` | A folder may also appear inside folders other than its parent (`AddFolderShortcut`, Drive's shortcuts to folders). The parent owns it: the tree is never walked through a shortcut (`Path`, `Ancestors`, the acl `Hierarchy`, `Move`'s cycle check), so a shortcut gives no access and cannot make a cycle; moving the folder where it has a shortcut removes that shortcut; deleting a folder removes the shortcuts in it and to it, and a folder of shortcuts only is empty | one place per folder; `AddFolderShortcut` is `unsupported` |
| `folders.WithClock(fn)` | The clock for `Folder.CreatedAt`, for tests | the Store's clock (`records.WithClock`) |

## Tables

`fs_folders` and `fs_folder_files` (migration `folders/0001_folders`):

| Table | Columns | Indexes |
|---|---|---|
| `fs_folders` | `id` (primary key, `fld_…`), `scope`, `parent_id` (`""` for top level), `name`, `created_at` | `fs_folders_sibling` unique `(scope, parent_id, name)` |
| `fs_folder_files` | `file_id` (primary key), `scope`, `folder_id` | `fs_folder_files_folder (scope, folder_id)` |
| `fs_folder_locks` | `scope` (primary key), `version` (migration `0002_folder_locks`) | |
| `fs_folder_shortcuts` | `scope`, `folder_id`, `file_id` (primary key together), `created_at` (migration `0003_folder_shortcuts`) | `fs_folder_shortcuts_file (scope, file_id)` |
| `fs_folder_folder_shortcuts` | `scope`, `folder_id` (where it is shown), `target_id` (the folder shown) (primary key together), `created_at` (migration `0004_folder_folder_shortcuts`) | `fs_folder_folder_shortcuts_target (scope, target_id)` |

Typed columns for queries of your own: `folders.Folders`, `ID`, `Scope`, `ParentID`, `Name`, `CreatedAt`; `folders.Places`, `PlaceScope`, `PlaceFileID`, `PlaceFolderID`.

## Go API

| Function or method | What it does | Errors |
|---|---|---|
| `folders.New(opts...)` | Returns the plugin | |
| `folders.CheckName(name)` | Reports whether `name` can name a folder | `invalid_argument` |
| [`(*Plugin).Create(ctx, store, scope, parent, name)`](ds:block?id=maketree-y2vgw4k8) | Makes a folder in `parent` (`RootID` for the top level) | `invalid_argument` (name, depth), `exists` (sibling name), `not_found` (parent) |
| `(*Plugin).Folder(ctx, store, scope, id)` | One folder | `not_found` |
| [`(*Plugin).Path(ctx, store, scope, id)`](ds:block?id=breadcrumbs-v8yjxw4t) | The folders from the top level down to `id`, for breadcrumbs; empty for the root | `not_found`, `unknown` (cycle) |
| `(*Plugin).Children(ctx, store, scope, parent)` | The folders directly inside `parent`, by name | |
| `(*Plugin).Rename(ctx, store, scope, id, name)` | Renames a folder; the current name is a no-op | `invalid_argument`, `exists`, `not_found` |
| [`(*Plugin).Move(ctx, store, scope, id, parent)`](ds:block?id=refusedmoves-2bvp46qf) | Moves a folder with its contents under `parent` | `invalid_argument` (into itself, too deep), `exists`, `not_found` |
| [`(*Plugin).Delete(ctx, store, scope, id)`](ds:block?id=emptyanddelete-9738wmwq) | Removes an empty folder; its trashed files move to the root | `invalid_argument` (not empty), `not_found` |
| [`(*Plugin).Put(ctx, store, scope, fileID, folder)`](ds:block?id=uploadinto-dszbu3j3) | Moves a file into `folder` (`RootID` for the root) | `not_found` |
| `(*Plugin).FolderFacet(ctx, store, fq)` | Counts the files `fq` selects by the folder each is directly in (`Value` the folder ID, `""` for the root), as `records.Store.Facets` | `invalid_argument` (`Limit` or `Cursor` set) |
| `(*Plugin).FolderOf(ctx, store, scope, fileID)` | The folder a file is in (its home), `RootID` for the root | `not_found` (file) |
| `(*Plugin).AddShortcut(ctx, store, scope, fileID, folder)`, `RemoveShortcut(…)` | Shows a live file in another folder too (`WithShortcuts`), or stops; adding twice or removing a missing one does nothing | `unsupported` (no option), `invalid_argument` (the home), `not_found` (file trashed or unknown, folder) |
| `(*Plugin).ShortcutsOf(ctx, store, scope, fileID)`, `Linked(ctx, store, folder, fq)` | The folders a file has a shortcut in; the files with a shortcut in a folder, paged as `Files` | `not_found` (folder) |
| `(*Plugin).AddFolderShortcut(ctx, store, scope, target, into)`, `RemoveFolderShortcut(…)` | Shows a folder inside another folder too (`WithFolderShortcuts`), or stops; adding twice or removing a missing one does nothing | `unsupported` (no option), `invalid_argument` (its parent, itself or a folder inside it), `not_found` (either folder) |
| `(*Plugin).FolderShortcutsIn(ctx, store, scope, into)`, `FolderShortcutsOf(ctx, store, scope, target)` | The folders shown in a folder through shortcuts, by name (list them beside `Children`); the folders a folder has a shortcut in | |
| [`(*Plugin).Files(ctx, store, folder, fq)`](ds:block?id=listfolder-9ydubcfn) | The files directly in `folder`, with the order, pagination and trash filter of `records.Store.Files` | `not_found` (folder), `invalid_argument` (limit, cursor) |
| `BeforeCreate`, `AfterCreate`, `BeforePurge`, `Migrations`, `Name` | The `records.FileHooks`, `records.Migrator` and `records.Plugin` implementations the Store calls | |

| Constant | Value |
|---|---|
| `PluginName` | `"folders"` |
| `TableNameFolders`, `TableNamePlaces`, `TableNameLocks`, `TableNameShortcuts` | `"fs_folders"`, `"fs_folder_files"`, `"fs_folder_locks"`, `"fs_folder_shortcuts"` |
| `RootID` | `""` |
| `IDPrefix` | `"fld_"` |
| `MaxNameChars` | 255 |
| `MaxDepth` | 32 |
