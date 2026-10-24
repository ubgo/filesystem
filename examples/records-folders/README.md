# Folders for a drive-style app

The folders plugin of the records layer: a folder tree kept in the database, as a team drive or a document manager shows it, while the storage keys stay flat and random. This program builds a nested tree, renders breadcrumbs, uploads files into a folder and lists it page by page, renames a folder, tries the moves the plugin refuses, moves files out, deletes a folder, and checks which folder names are accepted. It uses `github.com/ubgo/filesystem/records` and `records/folders` on SQLite over an in-memory disk. The guide [Folders](../../docs/plugins/folders.md) explains each step.

## Run it

```sh
cd examples
go run ./records-folders
go test ./records-folders
```

It needs no network or environment variables. The SQLite database lives in a temporary directory that is deleted when the program exits, and the bytes live in memory.

## What it does

```sh
go run ./records-folders
```

```text
breadcrumbs: My Drive / Projects / Website / Assets
Assets holds [icons.txt hero.txt logo.txt] (2 pages)
move into own subfolder: invalid_argument; move beside a same-named folder: exists
icons.txt is in Projects: true
delete with logo.txt inside: invalid_argument; after trashing it: deleted, logo.txt in the root: true
Website has 0 subfolders
names: ["Reports 2026"=ok "a/b"=invalid_argument ".."=invalid_argument " padded"=invalid_argument]
byte for byte: create docs beside Docs: ok; rename Reports to DOCS: ok; rename Docs to DOCS: exists
FoldCase: create docs beside Docs: exists; rename Reports to DOCS: exists; rename Docs to DOCS: ok
shortcuts after linking: Q4 [report.txt], Board pack [report.txt (shortcut)]
shortcuts of report.txt: 1, in Board pack: true
shortcut into its home: invalid_argument
shortcuts in the trash: Q4 [empty], Board pack [empty]
shortcuts restored: Q4 [report.txt], Board pack [report.txt (shortcut)]
shortcuts after moving its home into Board pack: Q4 [empty], Board pack [report.txt]
shortcut without WithShortcuts: unsupported
```

1. Three nested folders are created, each inside the one before; `folders.RootID` is the parent of a top-level folder, and the breadcrumbs read from the top down.
2. Three files are uploaded into `Assets`, each upload and placement in one transaction, and the folder is listed newest first, two per page.
3. `Assets` is renamed to `Images`. Moving a folder into its own subtree is `invalid_argument`; moving one beside a folder of the same name is `exists`.
4. Files move with `Put`: one into `Projects`, one to the root.
5. Deleting a folder with a live file inside is refused. Once that file is trashed the folder can be deleted, and the trashed file moves to the root so a later restore never points into a deleted folder.
6. `folders.CheckName` accepts an ordinary name and refuses a slash, `..` and leading spaces.
7. Names are compared byte for byte by default, so `docs` can sit beside `Docs` (and `Docs` cannot be renamed to `DOCS` only because the earlier rename already took that name). A plugin made `folders.WithNameKey(folders.FoldCase)` (`casefold.go`) refuses a case variant of a sibling's name with `exists`, on create and on rename, while a folder may still be renamed to a case variant of its own name.
8. A plugin made `folders.WithShortcuts()` (`shortcuts.go`) shows one file in two folders: `report.txt` lives in `Q4` and `AddShortcut` links it into `Board pack`. `Files` lists a folder's homes and `Linked` what is linked into it; `ShortcutsOf` names the folders a file is linked into, and a shortcut into the file's own home is `invalid_argument`. Trashing the file takes it out of both listings and restoring brings it back to both. Moving its home into `Board pack` with `Put` removes the shortcut there, so the file is never listed twice in one folder. A plugin without the option answers `AddShortcut` with `unsupported`.

## The code

Installing the plugin; `Migrate` creates `fs_folders` and `fs_folder_files`:

<!-- ds:block id=newstore-q3g7kahh -->
**newStore** · [`examples/records-folders/main.go:72-81`](main.go#L72-L81)

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

The tree and its breadcrumbs:

<!-- ds:block id=maketree-y2vgw4k8 -->
**makeTree** · [`examples/records-folders/main.go:89-100`](main.go#L89-L100)

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

<!-- ds:block id=breadcrumbs-v8yjxw4t -->
**breadcrumbs** · [`examples/records-folders/main.go:104-114`](main.go#L104-L114)

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

Uploading into a folder in one transaction, so a failed placement leaves no file in the root, and listing a folder with the Store's order and cursors:

<!-- ds:block id=uploadinto-dszbu3j3 -->
**uploadInto** · [`examples/records-folders/main.go:119-129`](main.go#L119-L129)

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
**listFolder** · [`examples/records-folders/main.go:134-150`](main.go#L134-L150)

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

The moves the plugin refuses, deleting a folder, and name checks:

<!-- ds:block id=refusedmoves-2bvp46qf -->
**refusedMoves** · [`examples/records-folders/main.go:155-162`](main.go#L155-L162)

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

<!-- ds:block id=emptyanddelete-9738wmwq -->
**emptyAndDelete** · [`examples/records-folders/main.go:168-178`](main.go#L168-L178)

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

<!-- ds:block id=checknames-etmkda6d -->
**checkNames** · [`examples/records-folders/main.go:186-196`](main.go#L186-L196)

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

<!-- ds:block id=demodelete-7zswsmpu -->
**demoDelete** · [`examples/records-folders/main.go:258-282`](main.go#L258-L282)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: tree, breadcrumbs, uploads into a folder, listing, rename, moves, delete, names |
| `casefold.go` | Case-insensitive sibling names with `WithNameKey(FoldCase)`, beside the default |
| `shortcuts.go` | One file in several folders with `WithShortcuts`; code: [`linkInto`](ds:block?id=linkinto-c8v2m6pz), [`folderView`](ds:block?id=folderview-h4n9t2qw) |
| `main_test.go` | Runs the same steps, plus the maximum depth, a purge removing a file's place, rename and delete errors, and both name models |

## Next

- The guide: [Folders](../../docs/plugins/folders.md).
- Related examples: [recipe-team-drive](../recipe-team-drive) for a whole drive, [records-tags](../records-tags) for a second way to organise files, [records-share](../records-share) to share one file by link.
