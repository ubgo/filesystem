# Folders

You will have virtual folders: a tree of named folders per scope, a place in it for every file, breadcrumbs, a folder listing with pagination, and renames and moves that are instant on every backend. It is the "My Drive" of a drive, a document manager or a media library. Install it when users organise files themselves; if files are only ever reached through what they belong to (a post's gallery, a user's avatar), [attachments](../records/attachments.md) are the better model.

## Before you start

- A Store, as in [Files](../records/files.md). The plugin needs no other plugin.
- The example is [`examples/records-folders`](../../examples/records-folders): `go run ./records-folders` in `examples/` prints every step below; `go test ./records-folders` runs them.
- Folders exist only in the database. A file's storage key never changes when it moves, so moving a file, or a folder with ten thousand files in it, updates one row and touches no object. The flip side: the bucket's layout does not mirror the folder tree, and tools that browse the bucket directly see flat keys.

## Step 1 — Install the plugin

Pass `folders.New()` to `records.WithPlugins`. `Migrate` creates `fs_folders` (the tree) and `fs_folder_files` (which folder each file is in).

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

`folders.WithClock(fn)` sets the clock for folder creation times, for tests; without it the plugin reads the Store's clock, so `records.WithClock` alone fakes time for the Store and the plugin. Folder IDs come from the Store's generator (`records.WithIDs`, through `Store.NewID`).

## Step 2 — Build a tree

`Create(ctx, store, scope, parent, name)` makes a folder in `parent`. The root of every scope is `folders.RootID` (`""`); it has no row, and a top-level folder's `ParentID` is `RootID`.

<!-- ds:block id=maketree-y2vgw4k8 -->
**makeTree** · [`examples/records-folders/main.go:89-100`](../../examples/records-folders/main.go#L89-L100)

```go
func makeTree(ctx context.Context, s *records.Store, fp *folders.Plugin) (tree, error) {
	var t tree
	var err error
	if t.projects, err = fp.Create(ctx, s, tenant, folders.RootID, "Projects"); err != nil {
		return t, err
	}
	if t.website, err = fp.Create(ctx, s, tenant, t.projects.ID, "Website"); err != nil {
		return t, err
	}
	t.assets, err = fp.Create(ctx, s, tenant, t.website.ID, "Assets")
	return t, err
}
```
<!-- /ds:block hash=af7d9a -->

Names are unique among siblings, compared byte for byte on every engine (on MySQL the table uses `utf8mb4_bin`), so `Reports` and `reports` can live side by side. A second sibling with the same name is `exists`. A parent that does not exist in the scope is `not_found`.

Folders nest at most `folders.MaxDepth` (32) deep, counting a top-level folder as depth 1; creating the 33rd level is `invalid_argument`.

## Step 3 — Breadcrumbs

`Path(ctx, store, scope, id)` returns the folders from the top level down to `id`; the root's path is empty.

<!-- ds:block id=breadcrumbs-v8yjxw4t -->
**breadcrumbs** · [`examples/records-folders/main.go:104-114`](../../examples/records-folders/main.go#L104-L114)

```go
func breadcrumbs(ctx context.Context, s *records.Store, fp *folders.Plugin, id string) (string, error) {
	path, err := fp.Path(ctx, s, tenant, id)
	if err != nil {
		return "", err
	}
	names := []string{"My Drive"}
	for _, f := range path {
		names = append(names, f.Name)
	}
	return strings.Join(names, " / "), nil
}
```
<!-- /ds:block hash=c52782 -->

Check: the program prints `breadcrumbs: My Drive / Projects / Website / Assets`. "My Drive" is the app's name for the root, not something the plugin stores.

`Children(ctx, store, scope, parent)` lists the folders directly inside `parent` (`RootID` for the top level), sorted by name. `Folder(ctx, store, scope, id)` returns one folder, or `not_found`.

## Step 4 — Put files in folders

`Put(ctx, store, scope, fileID, folder)` moves a file into a folder, or back to the root with `RootID`. A file that was never put anywhere is in the root. `FolderOf(ctx, store, scope, fileID)` returns the folder a file is in (`RootID` for the root). Uploading straight into a folder is an upload and a `Put` in one transaction, so a failed `Put` leaves no stray file in the root:

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

`Put` checks that the file and the folder exist in the scope (`not_found` otherwise). A trashed file can be put in a folder too.

## Step 5 — List a folder

`Files(ctx, store, folder, fq)` lists the files directly in a folder with exactly the order, pagination and trash filter of `records.Store.Files` (`fq.Scope` is the scope; `fq.Trashed` lists the folder's trashed files instead). Under the hood it adds one condition to `fq.Filters`, so you can combine it with filters of your own, such as a [tag filter](tags.md).

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

Check: with a page size of 2, the program prints `Assets holds [icons.txt hero.txt logo.txt] (2 pages)`, newest first. `Files` on the root lists the files that are in no folder. Subfolders are not files: list them with `Children` and show them above the files.

## Step 6 — Rename and move

`Rename(ctx, store, scope, id, name)` renames a folder; a sibling with the new name is `exists`, and renaming to the current name succeeds. `Move(ctx, store, scope, id, parent)` puts a folder, with everything in it, under another parent (`RootID` for the top level). It refuses, with nothing changed:

<!-- ds:block id=refusedmoves-2bvp46qf -->
**refusedMoves** · [`examples/records-folders/main.go:155-162`](../../examples/records-folders/main.go#L155-L162)

```go
func refusedMoves(ctx context.Context, s *records.Store, fp *folders.Plugin, t tree) (intoSelf, clash filesystem.Code, err error) {
	intoSelf = filesystem.CodeOf(fp.Move(ctx, s, tenant, t.website.ID, t.assets.ID))
	if _, err = fp.Create(ctx, s, tenant, folders.RootID, "Website"); err != nil {
		return "", "", err
	}
	clash = filesystem.CodeOf(fp.Move(ctx, s, tenant, t.website.ID, folders.RootID))
	return intoSelf, clash, nil
}
```
<!-- /ds:block hash=c8d1f6 -->

Check: `move into own subfolder: invalid_argument; move beside a same-named folder: exists`. A move that would make the subtree deeper than `MaxDepth` is `invalid_argument` too.

Two moves racing each other in separate transactions (A into B while B moves into A) cannot form a cycle. Every change to the shape of a scope's tree (`Create`, `Move`, `Delete`, `Put`) first takes that scope's row in `fs_folder_locks`, so they run one at a time per scope: the second move waits for the first to commit, then sees A inside B and is refused as a move into itself (`invalid_argument`). The same lock stops a `Put` into a folder that a racing `Delete` removes. It works the same on SQLite, Postgres and MySQL. One rule on MySQL, whose default isolation (REPEATABLE READ) reads from the snapshot of a transaction's first read: when you call these methods inside a `store.Tx` of your own, call them before the transaction reads anything else, or they check against that older snapshot. Walks are still capped at `MaxDepth`, so a cycle written to the tables behind the plugin's back is an `unknown` error, never a hang.

## Step 7 — Delete a folder

`Delete(ctx, store, scope, id)` removes an empty folder. A folder holding subfolders or live files is `invalid_argument`; delete or move its contents first (the plugin never deletes files on your behalf). Files of the folder that are in the trash do not block it: they move to the root, so restoring one later never points into a folder that is gone.

<!-- ds:block id=emptyanddelete-9738wmwq -->
**emptyAndDelete** · [`examples/records-folders/main.go:168-178`](../../examples/records-folders/main.go#L168-L178)

```go
func emptyAndDelete(ctx context.Context, s *records.Store, fp *folders.Plugin, folder string, f records.File) (refused filesystem.Code, after string, err error) {
	refused = filesystem.CodeOf(fp.Delete(ctx, s, tenant, folder))
	if err = s.Trash(ctx, tenant, f.ID); err != nil {
		return "", "", err
	}
	if err = fp.Delete(ctx, s, tenant, folder); err != nil {
		return "", "", err
	}
	after, err = fp.FolderOf(ctx, s, tenant, f.ID)
	return refused, after, err
}
```
<!-- /ds:block hash=172070 -->

The demo moves two files out first, then deletes:

<!-- ds:block id=demodelete-7zswsmpu -->
**demoDelete** · [`examples/records-folders/main.go:258-282`](../../examples/records-folders/main.go#L258-L282)

```go
func demoDelete(ctx context.Context, w io.Writer, s *records.Store, fp *folders.Plugin, t tree, files []records.File) error {
	if err := fp.Put(ctx, s, tenant, files[2].ID, t.projects.ID); err != nil {
		return err
	}
	if err := fp.Put(ctx, s, tenant, files[1].ID, folders.RootID); err != nil {
		return err
	}
	where, err := fp.FolderOf(ctx, s, tenant, files[2].ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "icons.txt is in Projects: %t\n", where == t.projects.ID)
	refused, after, err := emptyAndDelete(ctx, s, fp, t.assets.ID, files[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "delete with logo.txt inside: %s; after trashing it: deleted, logo.txt in the root: %t\n", refused, after == folders.RootID)
	children, err := fp.Children(ctx, s, tenant, t.website.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Website has %d subfolders\n", len(children))
	fmt.Fprintln(w, "names:", checkNames("Reports 2026", "a/b", "..", " padded"))
	return nil
}
```
<!-- /ds:block hash=3663d5 -->

Check: `delete with logo.txt inside: invalid_argument; after trashing it: deleted, logo.txt in the root: true`. Deleting an unknown folder is `not_found`.

## Step 8 — Validate names before you store them

`folders.CheckName(name)` is what `Create` and `Rename` apply: 1 to 255 characters (`MaxNameChars`) of valid UTF-8, no `/`, no control characters, no leading or trailing spaces, and not `.` or `..`. These are the names that survive a download as a zip archive or a sync to a real file system. Call it in your form validation to report errors before any database work:

<!-- ds:block id=checknames-etmkda6d -->
**checkNames** · [`examples/records-folders/main.go:186-196`](../../examples/records-folders/main.go#L186-L196)

```go
func checkNames(names ...string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		result := nameOK
		if err := folders.CheckName(n); err != nil {
			result = string(filesystem.CodeOf(err))
		}
		out = append(out, fmt.Sprintf("%q=%s", n, result))
	}
	return out
}
```
<!-- /ds:block hash=f4cf22 -->

Check: `names: ["Reports 2026"=ok "a/b"=invalid_argument ".."=invalid_argument " padded"=invalid_argument]`.

## Case-insensitive names

By default sibling names are compared byte for byte, so "Docs" and "docs" can sit side by side. A product that syncs to Windows or macOS, or simply expects them to clash, passes `folders.WithNameKey(folders.FoldCase)`: a folder created, renamed or moved next to a sibling whose name has the same key is refused with `exists`. Names are still stored and shown as typed, and a folder may be renamed to a case variant of its own name. Any pure `func(name string) string` works as the key, for example one that also normalises Unicode forms with `golang.org/x/text/unicode/norm`:

```go
p := folders.New(folders.WithNameKey(func(name string) string {
	return strings.ToLower(norm.NFC.String(name))
}))
```

Folders that already clash when the option is turned on are left alone; only later changes are checked.

Try it: [`examples/records-folders`](../../examples/records-folders) runs this section in `casefold.go` ([`newCaseInsensitive`](ds:block?id=newcaseinsensitive-t5k2w8jr)): `go run ./records-folders` from `examples/`.

## One file in several folders: shortcuts

A file has one home folder. `folders.WithShortcuts()` lets it also appear in other folders, as Drive's shortcuts do: the same file, not a copy, shown in "Q4 report" and in "Board pack".

```go
p := folders.New(folders.WithShortcuts())

err := p.Put(ctx, store, scope, report.ID, q4.ID)              // its home
err = p.AddShortcut(ctx, store, scope, report.ID, boardPack.ID) // and shown here too
homes, err := p.Files(ctx, store, boardPack.ID, records.FileQuery{Scope: scope})  // files whose home it is
links, err := p.Linked(ctx, store, boardPack.ID, records.FileQuery{Scope: scope}) // files linked into it
in, err := p.ShortcutsOf(ctx, store, scope, report.ID)                            // [boardPack.ID]
```

The home folder keeps owning the file, and every operation follows from that:

| Operation | With shortcuts |
|---|---|
| `Put` (move) | Moves the home; the shortcuts stay. Moving the home into a folder where the file had a shortcut removes that shortcut, so a file is never listed twice in one folder. |
| Trash, restore | A trashed file leaves `Linked` as it leaves `Files` (`Trashed: true` lists it in both); restoring it brings it back to both. |
| Purge | Removes its shortcuts with it. |
| Delete a folder | Removes the shortcuts in it, never their files. A folder holding only shortcuts counts as empty. Moving a folder moves its shortcuts with it. |
| Permissions | A shortcut gives no access. `FolderOf`, which the [acl](acl.md) plugin's `Hierarchy` reads, answers the home only, so a grant on "Board pack" does not reach the report linked into it: give the readers a grant on the file. |
| Sharing | A folder link ([share](share.md)) shares the folder's homes only, never what is linked into it. |
| Counting | `FolderFacet` and quota count each file once, at its home. |

`AddShortcut` into the file's home is `invalid_argument`; on a trashed or unknown file, or an unknown folder, it is `not_found`; adding one that exists does nothing. `RemoveShortcut` works with or without the option, so shortcuts can always be cleared; without it `AddShortcut` is `unsupported`. Both emit `folders.shortcut_added` and `folders.shortcut_removed` (`FileID`, `Detail[DetailFolder]`) inside the change's transaction.

Try it: [`examples/records-folders`](../../examples/records-folders) runs this section in `shortcuts.go` ([`linkInto`](ds:block?id=linkinto-c8v2m6pz)): `go run ./records-folders` from `examples/`, then look for `shortcuts after linking: Q4 [report.txt], Board pack [report.txt (shortcut)]`.

## Shortcuts to folders

`folders.WithFolderShortcuts()` lets a folder appear inside folders other than its parent, as Drive's shortcuts to folders do. It is a separate option from `WithShortcuts`, and the same rule holds: the parent owns the folder, and a shortcut gives no access.

```go
p := folders.New(folders.WithFolderShortcuts())

err := p.AddFolderShortcut(ctx, store, scope, q4.ID, boardPack.ID) // "Q4" shown inside "Board pack" too
shown, err := p.FolderShortcutsIn(ctx, store, scope, boardPack.ID) // list beside Children
```

- **No access.** `Path` and `Ancestors`, which the acl plugin's `Hierarchy` reads, follow parents only, so a grant on "Board pack" reaches neither "Q4" nor its files; a folder share (`Collection`) shares the files whose home it is.
- **No cycles.** The tree is never walked through a shortcut (`Path`, `Ancestors`, `Move`'s cycle check, the depth cap), so a shortcut cannot make a cycle, even after a later `Move`.
- **Never twice in one place.** Moving the folder into a folder where it has a shortcut removes that shortcut. Deleting a folder removes the shortcuts in it and the shortcuts to it, and a folder holding only shortcuts counts as empty.

`AddFolderShortcut` into the folder's own parent, into itself or into a folder inside it is `invalid_argument`; an unknown folder is `not_found`; adding one that exists does nothing. `RemoveFolderShortcut` works with or without the option; without it `AddFolderShortcut` is `unsupported`. `FolderShortcutsOf` lists where a folder is shown. Both changes emit `folders.folder_shortcut_added` and `folders.folder_shortcut_removed` (`Subject` the folder shown, `Detail[DetailFolder]` where) inside the change's transaction.

## Count files by folder

`FolderFacet(ctx, store, fq)` counts the files a `records.FileQuery` selects (its `Scope`, `Trashed` and `Filters`) by the folder each is directly in, with their bytes, most files first: the folder facet beside the Store's content-type and visibility counts ([Files](../records/files.md#facet-counts)). It returns `[]records.FacetCount`; `Value` is the folder ID, `""` for files in the root. Subfolders are not added into their parents. `Limit` or `Cursor` set is `invalid_argument`.

## The tables

`fs_folders` and `fs_folder_files`, created by the migration `folders/0001_folders`, `fs_folder_locks`, created by `folders/0002_folder_locks`, `fs_folder_shortcuts`, created by `folders/0003_folder_shortcuts`, and `fs_folder_folder_shortcuts`, created by `folders/0004_folder_folder_shortcuts`:

| Table | Column | Holds |
|---|---|---|
| `fs_folders` | `id` | primary key, `fld_` + 26 random base32 characters |
| | `scope` | the scope |
| | `parent_id` | the parent folder, `""` for top level (never `NULL`, so sibling uniqueness holds on every engine) |
| | `name` | up to 255 characters |
| | `created_at` | UTC, microseconds |
| `fs_folder_files` | `file_id` | primary key: a file has at most one home folder |
| | `scope` | the scope |
| | `folder_id` | the folder; files in the root have no row |
| `fs_folder_locks` | `scope` | primary key: one row per scope whose tree has changed |
| | `version` | counts the changes of shape; bumping it (one upsert) is what locks the scope's tree until the transaction ends |
| `fs_folder_shortcuts` | `scope`, `folder_id`, `file_id` | primary key together: one shortcut of a file per folder (`WithShortcuts`) |
| | `created_at` | UTC, microseconds |
| `fs_folder_folder_shortcuts` | `scope`, `folder_id`, `target_id` | primary key together: folder `target_id` shown in folder `folder_id` (`WithFolderShortcuts`); indexed on `(scope, target_id)` |
| | `created_at` | UTC, microseconds |

Indexes: `fs_folders_sibling` unique on `(scope, parent_id, name)`; `fs_folder_files_folder` on `(scope, folder_id)`; `fs_folder_shortcuts_file` on `(scope, file_id)`.

## With other plugins and the core

- Trash and restore do not move files: a trashed file keeps its folder and comes back there, unless its folder was deleted meanwhile, in which case it is already in the root.
- Purge removes the file's place (`BeforePurge`), in the purge transaction.
- `Replace`, `SetVisibility` and versions keep the file's ID, so the file stays in its folder.
- Folders are per scope. `Put` refuses a folder of another scope with `not_found`, and so does every lookup.
- Every change emits a `records.Event` inside its transaction, through `Store.Emit`, so `EventHooks` plugins and webhooks see it: `folders.created`, `folders.renamed`, `folders.moved`, `folders.deleted` (`Subject` is the folder) and `folders.file_put` (`FileID`). `Detail` carries the new and old place or name under the keys `folders.DetailParent`, `DetailName`, `DetailFolder` and `DetailFrom`; the full table is in [Events and hooks](../records/events-and-hooks.md#events-from-plugins). A call that changes nothing (a rename to the current name, a move or `Put` to where it already is) emits nothing, and a hook's error rolls the change back. The [audit](audit.md) log writes one entry per event with its kind, file, actor, time, `Subject` and `Detail`.
- Combine a folder with [tags](tags.md) in one listing by putting both filters in one `records.FileQuery`; the folder filter is what `Files` adds, so call `store.Files` yourself with both.
- With [quota](quota.md), folders cost nothing: they are rows, not bytes.

## What can go wrong

- `exists` — a sibling already has that name (`Create`, `Rename`, `Move`). On Postgres the failed statement aborts the transaction, so if you called it inside your own `store.Tx`, that transaction must end too.
- `invalid_argument` — a bad name, a move into the folder's own subtree, a tree deeper than 32, or a delete of a non-empty folder.
- `not_found` — an unknown folder or file, or one of another scope.
- `unknown` with "deeper than 32, or in a cycle" — something wrote a cycle into `fs_folders` outside the plugin (concurrent moves through the plugin are serialized); repair by moving one folder back to the root.
- `not_found` from `Rename` for a folder you just listed: a `Delete` removed it in between; nothing was renamed and no event was emitted.
- On MySQL, a move inside your own transaction made a cycle or went too deep — the transaction read something before the move, so the move checked an old snapshot; call folder methods before other reads in that transaction.

## Next

- [Tags](tags.md): labels across folders, and the filter that combines with a folder listing.
- [Files](../records/files.md): `FileQuery`, cursors and filters, which `Files` builds on.
- [Lifecycle](../records/lifecycle.md): trash, restore and purge.
- The plugin's [README](../../records/folders/README.md) is the reference for its tables and Go API.
