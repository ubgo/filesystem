// Command records-move shows moving recorded files between disks one at a
// time with Store.MoveFile, for "accept fast, store later": uploads land
// on a fast local disk so the request returns at once, and a background
// job promotes them to the main disk. The file keeps its ID, key,
// attachments and URL routes; only where its bytes live changes.
//
//	go run ./records-move
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
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	tenant = "acme"
	// incoming is the fast disk uploads land on (a local SSD in
	// production); kept is the disk files live on (a bucket).
	incoming = "incoming"
	kept     = "kept"
)

var notes = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"text/plain"}, Key: upload.RandomKey("notes")}

func newStore(ctx context.Context, db *sql.DB) (*records.Store, error) {
	in, err := filesystem.NewDisk(incoming, memory.New())
	if err != nil {
		return nil, err
	}
	out, err := filesystem.NewDisk(kept, memory.New())
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(in, out)
	if err != nil {
		return nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks)
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// promote moves every file still on the incoming disk to the kept disk,
// one at a time, and returns how many it moved. Each move is its own
// transaction and is safe to run again, so a job runs it on a schedule
// and a crash mid-way only means the next run finishes the work.
// ds:def id=promote-4a2sna36
func promote(ctx context.Context, s *records.Store) (int, error) {
	moved := 0
	for {
		page, err := s.Files(ctx, records.FileQuery{
			Scope:   tenant,
			Filters: []sqlb.Predicate{sqlb.Eq(records.FileDisk, incoming)},
		})
		if err != nil {
			return moved, err
		}
		if len(page.Files) == 0 {
			return moved, nil
		}
		for _, f := range page.Files {
			if _, err := s.MoveFile(ctx, tenant, f.ID, kept); err != nil {
				return moved, fmt.Errorf("promote %s: %w", f.ID, err)
			}
			moved++
		}
	}
}

func run(ctx context.Context, w io.Writer, dir string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "records.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	s, err := newStore(ctx, db)
	if err != nil {
		return err
	}
	var ids []string
	for _, body := range []string{"first note", "second note", "third note"} {
		f, err := s.Upload(ctx, tenant, incoming, strings.NewReader(body), "note.txt", notes)
		if err != nil {
			return err
		}
		ids = append(ids, f.ID)
	}
	fmt.Fprintf(w, "uploaded %d notes to %q\n", len(ids), incoming)
	n, err := promote(ctx, s)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "promoted %d\n", n)
	f, err := s.File(ctx, tenant, ids[0])
	if err != nil {
		return err
	}
	in, _ := s.Disks().Disk(incoming)
	out, _ := s.Disks().Disk(kept)
	left, _ := in.Exists(ctx, f.Key)
	body, err := out.ReadAll(ctx, f.Key)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "first note: disk=%s, still on %s=%v, reads %q\n", f.Disk, incoming, left, body)
	n, err = promote(ctx, s)
	fmt.Fprintf(w, "promote again: %d, %v\n", n, err)
	return nil
}

func main() {
	dir, err := os.MkdirTemp("", "records-move")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
