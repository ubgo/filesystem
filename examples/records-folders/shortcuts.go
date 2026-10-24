package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/folders"
)

// linkInto shows a file in a second folder with a shortcut, as Drive
// does: the same file, not a copy. Its home stays where Put placed it.
// ds:def id=linkinto-c8v2m6pz
func linkInto(ctx context.Context, s *records.Store, fp *folders.Plugin, fileID, folder string) error {
	return fp.AddShortcut(ctx, s, tenant, fileID, folder)
}

// folderView lists a folder as a file browser shows it: the files whose
// home it is, then the files linked into it.
// ds:def id=folderview-h4n9t2qw
func folderView(ctx context.Context, s *records.Store, fp *folders.Plugin, folder string) (string, error) {
	q := records.FileQuery{Scope: tenant}
	homes, err := fp.Files(ctx, s, folder, q)
	if err != nil {
		return "", err
	}
	links, err := fp.Linked(ctx, s, folder, q)
	if err != nil {
		return "", err
	}
	var names []string
	for _, f := range homes.Files {
		names = append(names, f.Filename)
	}
	for _, f := range links.Files {
		names = append(names, f.Filename+" (shortcut)")
	}
	if len(names) == 0 {
		return "empty", nil
	}
	return strings.Join(names, ", "), nil
}

// shortcuts puts a report in "Q4" and links it into "Board pack", then
// moves, trashes and restores it to show the home owning the file.
func shortcuts(ctx context.Context, w io.Writer, dir string) error {
	db, err := openSQLite(filepath.Join(dir, "shortcuts.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	disks, err := newDisks()
	if err != nil {
		return err
	}
	fp := folders.New(folders.WithShortcuts())
	s, err := newStore(ctx, db, disks, fp)
	if err != nil {
		return err
	}
	q4, err := fp.Create(ctx, s, tenant, folders.RootID, "Q4")
	if err != nil {
		return err
	}
	board, err := fp.Create(ctx, s, tenant, folders.RootID, "Board pack")
	if err != nil {
		return err
	}
	report, err := uploadInto(ctx, s, fp, q4.ID, "report.txt")
	if err != nil {
		return err
	}
	if err := linkInto(ctx, s, fp, report.ID, board.ID); err != nil {
		return err
	}
	view := func(label string) error {
		inQ4, err := folderView(ctx, s, fp, q4.ID)
		if err != nil {
			return err
		}
		inBoard, err := folderView(ctx, s, fp, board.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "shortcuts %s: Q4 [%s], Board pack [%s]\n", label, inQ4, inBoard)
		return nil
	}
	if err := view("after linking"); err != nil {
		return err
	}
	in, err := fp.ShortcutsOf(ctx, s, tenant, report.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "shortcuts of report.txt: %d, in Board pack: %t\n", len(in), len(in) == 1 && in[0] == board.ID)
	fmt.Fprintf(w, "shortcut into its home: %s\n", filesystem.CodeOf(linkInto(ctx, s, fp, report.ID, q4.ID)))
	if err := s.Trash(ctx, tenant, report.ID); err != nil {
		return err
	}
	if err := view("in the trash"); err != nil {
		return err
	}
	if err := s.Restore(ctx, tenant, report.ID); err != nil {
		return err
	}
	if err := view("restored"); err != nil {
		return err
	}
	if err := fp.Put(ctx, s, tenant, report.ID, board.ID); err != nil {
		return err
	}
	if err := view("after moving its home into Board pack"); err != nil {
		return err
	}
	plainFolders := folders.New()
	plain, err := newStore(ctx, db, disks, plainFolders)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "shortcut without WithShortcuts: %s\n", filesystem.CodeOf(plainFolders.AddShortcut(ctx, plain, tenant, report.ID, q4.ID)))
	return nil
}

// caseNamesThenShortcuts is run's last two steps, each on a Store of its
// own: names compared without case, then shortcuts.
func caseNamesThenShortcuts(ctx context.Context, w io.Writer, dir string) error {
	if err := caseNames(ctx, w, dir); err != nil {
		return err
	}
	return shortcuts(ctx, w, dir)
}
