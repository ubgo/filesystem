// Command records-folders shows the folders plugin of the records Store on
// SQLite and an in-memory disk: building a folder tree, breadcrumbs,
// putting files in folders and listing a folder page by page, renaming,
// moving (and the moves it refuses), deleting a folder, and the names it
// accepts.
//
//	go run ./records-folders
//
// It prints what each step returned; main_test.go runs the same steps.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	// diskName is the one disk files are stored on.
	diskName = "drive"
	// tenant is the scope every folder and file of the demo belongs to.
	tenant = "acme"
	// maxUpload caps one upload.
	maxUpload = 1 << 20
	// folderPage is the page size of a folder listing; tiny so pagination
	// shows.
	folderPage = 2
)

// textPolicy accepts plain text with a random key. The key never changes
// when a file moves between folders: folders live only in the database.
var textPolicy = upload.Policy{MaxSize: maxUpload, Allowed: []string{"text/plain"}, Key: upload.RandomKey("files")}

// newDisks builds the manager the Store reaches bytes through: one memory disk.
func newDisks() (*filesystem.Manager, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, err
	}
	return filesystem.NewManager(disk)
}

// openSQLite opens a SQLite database file with one connection (SQLite has
// one writer) and a busy timeout.
func openSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// newStore installs the folders plugin and migrates, which creates
// fs_folders and fs_folder_files.
// ds:def id=newstore-q3g7kahh
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

// tree is the demo's folder tree: Projects/Website/Assets.
type tree struct{ projects, website, assets folders.Folder }

// makeTree creates three nested folders, each inside the one before;
// folders.RootID is the parent of a top-level folder.
// ds:def id=maketree-y2vgw4k8
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

// breadcrumbs renders a folder's path from the top level down.
// ds:def id=breadcrumbs-v8yjxw4t
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

// uploadInto uploads a file and puts it in folder, in one transaction, so
// a failed Put leaves no file in the root.
// ds:def id=uploadinto-dszbu3j3
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

// listFolder walks every page of a folder's live files, newest first,
// with the order and cursors of records.Store.Files.
// ds:def id=listfolder-9ydubcfn
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

// refusedMoves tries the moves the plugin refuses and returns their
// error codes: a folder into its own subtree, and a sibling name clash.
// ds:def id=refusedmoves-2bvp46qf
func refusedMoves(ctx context.Context, s *records.Store, fp *folders.Plugin, t tree) (intoSelf, clash filesystem.Code, err error) {
	intoSelf = filesystem.CodeOf(fp.Move(ctx, s, tenant, t.website.ID, t.assets.ID))
	if _, err = fp.Create(ctx, s, tenant, folders.RootID, "Website"); err != nil {
		return "", "", err
	}
	clash = filesystem.CodeOf(fp.Move(ctx, s, tenant, t.website.ID, folders.RootID))
	return intoSelf, clash, nil
}

// emptyAndDelete shows Delete: refused while a live file is inside,
// allowed once the file is trashed, which moves the trashed file to the
// root so a later Restore never points into a deleted folder.
// ds:def id=emptyanddelete-9738wmwq
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

// nameOK is what checkNames reports for an accepted name.
const nameOK = "ok"

// checkNames runs folders.CheckName over names a user might type and
// reports each one's result: nameOK or the refusal's error code.
// ds:def id=checknames-etmkda6d
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

// run performs every step and prints what it got.
func run(ctx context.Context, w io.Writer, dir string) error {
	db, err := openSQLite(filepath.Join(dir, "records.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	disks, err := newDisks()
	if err != nil {
		return err
	}
	fp := folders.New()
	s, err := newStore(ctx, db, disks, fp)
	if err != nil {
		return err
	}
	if err := demo(ctx, w, s, fp); err != nil {
		return err
	}
	return caseNamesThenShortcuts(ctx, w, dir)
}

// demo is run's body once the Store exists.
func demo(ctx context.Context, w io.Writer, s *records.Store, fp *folders.Plugin) error {
	t, err := makeTree(ctx, s, fp)
	if err != nil {
		return err
	}
	crumbs, err := breadcrumbs(ctx, s, fp, t.assets.ID)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "breadcrumbs:", crumbs)
	var files []records.File
	for _, name := range []string{"logo.txt", "hero.txt", "icons.txt"} {
		f, err := uploadInto(ctx, s, fp, t.assets.ID, name)
		if err != nil {
			return err
		}
		files = append(files, f)
	}
	names, pages, err := listFolder(ctx, s, fp, t.assets.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Assets holds %v (%d pages)\n", names, pages)
	if err := fp.Rename(ctx, s, tenant, t.assets.ID, "Images"); err != nil {
		return err
	}
	intoSelf, clash, err := refusedMoves(ctx, s, fp, t)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "move into own subfolder: %s; move beside a same-named folder: %s\n", intoSelf, clash)
	return demoDelete(ctx, w, s, fp, t, files)
}

// demoDelete is the second half of demo: moving files out of a folder,
// deleting it, and checking names.
// ds:def id=demodelete-7zswsmpu
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

func main() {
	dir, err := os.MkdirTemp("", "records-folders")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
