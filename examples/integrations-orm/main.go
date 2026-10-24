// Command integrations-orm reads the records tables (fs_files,
// fs_attachments) with the data layers an application already uses:
// sqlc's generated queries (contrib/sqlc), GORM models (contrib/gorm) and
// an ent client generated from contrib/ent's mixins. A records.Store
// writes every row; the three readers only read, over the same SQLite
// database, and each answers what the Store answers.
//
//	go run ./integrations-orm
//
// The fsdb package is what sqlc v1.31.1 generates from contrib/sqlc's
// sqlite directory, and the ent package is what ent v0.14.6 generates
// from ent/schema, two schemas that mix in contrib/ent (see README.md);
// main_test.go checks fsdb still covers every query the module ships.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/examples/integrations-orm/fsdb"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this app, not of the library.
const (
	// tenant is the records scope every row of the demo lives in.
	tenant = "acme"
	// diskName is the one disk the Store writes to.
	diskName = "media"
	// ownerPost is the owner type the gallery hangs off.
	ownerPost = "post"
	// postID is the post whose gallery is read.
	postID = "p1"
	// roleGallery is the attachment role of the gallery.
	roleGallery = "gallery"
	// trashedName is the gallery file moved to the trash, so every reader
	// must leave it out.
	trashedName = "draft.txt"
	// pageSize is the page length of the paginated sqlc listing.
	pageSize = 1
)

// galleryFiles are the files uploaded to the post's gallery, in position
// order.
var galleryFiles = []string{"cover.txt", trashedName, "map.txt"}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run writes the demo rows through a Store, then reads them with each
// data layer and prints what each one saw.
func run(ctx context.Context, out io.Writer) error {
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	// One connection, so every reader sees the one in-memory database.
	db.SetMaxOpenConns(1)

	store, err := openStore(ctx, db)
	if err != nil {
		return err
	}
	if err := seed(ctx, store); err != nil {
		return err
	}
	if err := readWithSqlc(ctx, out, db); err != nil {
		return err
	}
	if err := readWithGorm(out, db); err != nil {
		return err
	}
	return entThenPrefixes(ctx, out, db)
}

// openStore returns a migrated Store over db with one memory disk. The
// Store owns the tables: Migrate creates them, never an ORM.
func openStore(ctx context.Context, db *sql.DB) (*records.Store, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, err
	}
	mgr, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	store, err := records.New(db, sqlb.SQLiteDialect{}, mgr, records.WithOwnerTypes(ownerPost))
	if err != nil {
		return nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, err
	}
	return store, nil
}

// seed writes every row through the Store: three files attached to the
// post's gallery, the middle one then trashed. Writes never go through
// an ORM, so plugin hooks, events and object consistency all apply.
// ds:def id=seed-8tv7pj5w
func seed(ctx context.Context, store *records.Store) error {
	policy := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"text/plain"}}
	post := records.Owner{Type: ownerPost, ID: postID}
	for _, name := range galleryFiles {
		f, err := store.Upload(ctx, tenant, diskName, strings.NewReader(name), name, policy)
		if err != nil {
			return err
		}
		if _, err := store.Attach(ctx, tenant, f.ID, post, roleGallery); err != nil {
			return err
		}
		if name == trashedName {
			if err := store.Trash(ctx, tenant, f.ID); err != nil {
				return err
			}
		}
		// SQLite stores timestamps to the microsecond; a pause keeps the
		// newest-first order of the listings deterministic.
		time.Sleep(time.Millisecond)
	}
	return nil
}

// readWithSqlc prints the gallery, the live files page by page and the
// scope's totals through the sqlc-generated fsdb package.
func readWithSqlc(ctx context.Context, out io.Writer, db *sql.DB) error {
	q := fsdb.New(db)
	gallery, err := sqlcGallery(ctx, q, tenant, postID)
	if err != nil {
		return err
	}
	for _, a := range gallery {
		fmt.Fprintf(out, "sqlc gallery: %d %s %d bytes\n", a.Position, a.Filename, a.Size)
	}
	names, err := sqlcAllLiveFiles(ctx, q, tenant)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "sqlc live files, %d per page: %s\n", pageSize, strings.Join(names, ", "))
	totals, err := q.ScopeTotals(ctx, tenant)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "sqlc totals: %d files, %v bytes (trash included)\n", totals.Files, totals.Bytes)
	return nil
}

// sqlcGallery lists a post's gallery with the generated
// ListOwnerAttachments: the live files attached under the role, in
// position order, as Store.Attachments returns them. The query filters by
// scope first; never drop it, or a listing crosses tenants.
// ds:def id=sqlcgallery-udcqpkzw
func sqlcGallery(ctx context.Context, q *fsdb.Queries, scope, owner string) ([]fsdb.ListOwnerAttachmentsRow, error) {
	return q.ListOwnerAttachments(ctx, fsdb.ListOwnerAttachmentsParams{
		Scope:     scope,
		OwnerType: ownerPost,
		OwnerID:   owner,
		Role:      roleGallery,
	})
}

// sqlcAllLiveFiles walks every live file of a scope, newest first, with
// keyset pagination: ListLiveFiles for the first page, then
// ListLiveFilesAfter from the last row's (created_at, id) until a page
// comes back short. It is the order of Store.Files.
// ds:def id=sqlcalllivefiles-tgujm898
func sqlcAllLiveFiles(ctx context.Context, q *fsdb.Queries, scope string) ([]string, error) {
	page, err := q.ListLiveFiles(ctx, fsdb.ListLiveFilesParams{Scope: scope, PageSize: pageSize})
	var names []string
	for err == nil {
		for _, f := range page {
			names = append(names, f.Filename)
		}
		if len(page) < pageSize {
			return names, nil
		}
		last := page[len(page)-1]
		page, err = q.ListLiveFilesAfter(ctx, fsdb.ListLiveFilesAfterParams{
			Scope: scope, AfterCreatedAt: last.CreatedAt, AfterID: last.ID, PageSize: pageSize,
		})
	}
	return nil, err
}
