// Command records-versions shows the versions plugin of the records
// Store: replaced contents kept as versions, the cap that drops the
// oldest, listing, restoring and deleting a version, versions following
// their file to another disk when it is made public, purge taking them
// along, and the quota plugin charging only the current content.
//
//	go run ./records-versions
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
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/filesystem/records/versions"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	// publicDisk holds public files (a CDN bucket in production).
	publicDisk = "public"
	// privateDisk holds private files, reached through signed URLs only.
	privateDisk = "private"
	// tenant is the scope of every file of the demo.
	tenant = "acme"
	// keep is how many previous contents each file keeps.
	keep = 2
	// maxUpload caps one upload.
	maxUpload = 1 << 20
)

// textPolicy accepts plain text only. It names no visibility, so uploads
// are private and land on privateDisk.
var textPolicy = upload.Policy{MaxSize: maxUpload, Allowed: []string{"text/plain"}, Key: upload.RandomKey("docs")}

// newDisks builds two memory disks, one per visibility, the layout
// records.WithVisibilityDisks routes between.
// ds:def id=newdisks-8x2d8493
func newDisks() (*filesystem.Manager, error) {
	pub, err := filesystem.NewDisk(publicDisk, memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	if err != nil {
		return nil, err
	}
	priv, err := filesystem.NewDisk(privateDisk, memory.New())
	if err != nil {
		return nil, err
	}
	return filesystem.NewManager(priv, pub)
}

// openSQLite opens a SQLite database file with one connection (SQLite
// has one writer) and a busy timeout.
func openSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// unlimited is the quota of every scope in this demo: it only counts.
func unlimited(context.Context, string) (quota.Limit, error) { return quota.Limit{}, nil }

// newStore installs versions (keeping keep contents per file) and quota,
// routes files to a disk by visibility, and creates every table.
// ds:def id=newstore-2jkdgkez
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, *versions.Plugin, *quota.Plugin, error) {
	v, err := versions.New(versions.WithKeep(keep))
	if err != nil {
		return nil, nil, nil, err
	}
	q, err := quota.New(unlimited)
	if err != nil {
		return nil, nil, nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithVisibilityDisks(publicDisk, privateDisk), records.WithPlugins(v, q))
	if err != nil {
		return nil, nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, nil, err
	}
	return s, v, q, nil
}

// edit stores body as the file's new content. The versions plugin keeps
// the content it replaces (KeepVersion runs inside Replace's
// transaction) and drops the oldest past the cap, its object deleted
// after the commit.
// ds:def id=edit-3jmwen3q
func edit(ctx context.Context, s *records.Store, f records.File, body string) (records.File, error) {
	return s.Replace(ctx, tenant, f.ID, strings.NewReader(body), "", textPolicy)
}

// history lists a file's versions, newest first, as "number:size".
// ds:def id=history-nvyew4ud
func history(ctx context.Context, s *records.Store, v *versions.Plugin, fileID string) (string, []versions.Version, error) {
	list, err := v.Versions(ctx, s, tenant, fileID)
	if err != nil {
		return "", nil, err
	}
	parts := make([]string, 0, len(list))
	for _, ver := range list {
		parts = append(parts, fmt.Sprintf("v%d:%dB@%s", ver.Number, ver.Size, ver.Disk))
	}
	return strings.Join(parts, " "), list, nil
}

// restore makes a version the file's content again. Its object is
// copied, so the version stays, and the content it replaces becomes the
// newest version: a restore can itself be undone.
// ds:def id=restore-v7aa5ygn
func restore(ctx context.Context, s *records.Store, v *versions.Plugin, versionID string) (records.File, error) {
	ver, err := v.Version(ctx, s, tenant, versionID)
	if err != nil {
		return records.File{}, err
	}
	return v.Restore(ctx, s, tenant, ver.ID)
}

// forget deletes one version, its object after the commit.
// ds:def id=forget-jw43mckv
func forget(ctx context.Context, s *records.Store, v *versions.Plugin, versionID string) error {
	return v.Delete(ctx, s, tenant, versionID)
}

// publish makes the file public. With WithVisibilityDisks its object
// moves to publicDisk, and the plugin moves every kept version with it,
// so no old content stays behind on the other disk.
// ds:def id=publish-3ffb8xnh
func publish(ctx context.Context, s *records.Store, f records.File) error {
	return s.SetVisibility(ctx, tenant, f.ID, filesystem.VisibilityPublic)
}

// exists reports whether a version's object is still stored.
func exists(ctx context.Context, s *records.Store, ver versions.Version) (bool, error) {
	disk, err := s.Disks().Disk(ver.Disk)
	if err != nil {
		return false, err
	}
	return disk.Exists(ctx, ver.Key)
}

// run wires the Store and performs every step.
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
	s, v, q, err := newStore(ctx, db, disks)
	if err != nil {
		return err
	}
	if err := demo(ctx, w, s, v, q); err != nil {
		return err
	}
	return planHistory(ctx, w, dir)
}

// demo edits one file three times and prints its history.
func demo(ctx context.Context, w io.Writer, s *records.Store, v *versions.Plugin, q *quota.Plugin) error {
	f, err := s.Upload(ctx, tenant, "", strings.NewReader("a"), "notes.txt", textPolicy)
	if err != nil {
		return err
	}
	_, first, err := history(ctx, s, v, f.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "uploaded on %s, versions: %d\n", f.Disk, len(first))
	var dropped versions.Version
	for i, body := range []string{"bb", "ccc", "dddd"} {
		if f, err = edit(ctx, s, f, body); err != nil {
			return err
		}
		line, list, err := history(ctx, s, v, f.ID)
		if err != nil {
			return err
		}
		if i == 0 {
			dropped = list[0]
		}
		fmt.Fprintf(w, "edit %d -> %dB, history: %s\n", i+1, f.Size, line)
	}
	still, err := exists(ctx, s, dropped)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "v1 dropped past the cap, object still stored: %t\n", still)
	u, err := q.Usage(ctx, s, tenant)
	fmt.Fprintf(w, "quota counts %d bytes (current content only)\n", u.Bytes)
	if err != nil {
		return err
	}
	return later(ctx, w, s, v, f)
}

// later restores, deletes, publishes and purges.
func later(ctx context.Context, w io.Writer, s *records.Store, v *versions.Plugin, f records.File) error {
	_, list, err := history(ctx, s, v, f.ID)
	if err != nil {
		return err
	}
	oldest := list[len(list)-1]
	if f, err = restore(ctx, s, v, oldest.ID); err != nil {
		return err
	}
	line, list, err := history(ctx, s, v, f.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "restored v%d -> %dB, history: %s\n", oldest.Number, f.Size, line)
	if err := forget(ctx, s, v, list[len(list)-1].ID); err != nil {
		return err
	}
	if err := publish(ctx, s, f); err != nil {
		return err
	}
	line, list, err = history(ctx, s, v, f.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "deleted one, then published: history: %s\n", line)
	if err := s.Purge(ctx, tenant, f.ID); err != nil {
		return err
	}
	line, _, err = history(ctx, s, v, f.ID)
	if err != nil {
		return err
	}
	gone, err := exists(ctx, s, list[0])
	fmt.Fprintf(w, "purged: history %q, newest version object still stored: %t\n", line, gone)
	return err
}

func main() {
	dir, err := os.MkdirTemp("", "records-versions")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
