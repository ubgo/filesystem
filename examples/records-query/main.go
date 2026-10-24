// Command records-query shows the records Store's query and bulk API on
// SQLite and an in-memory disk: a role capped at a number of files
// (WithRoleLimit), an owner's files listed and filtered like any listing
// (AttachedTo), an owner's attachments a page at a time (AttachmentsPage),
// the facet counts of a media library's sidebar (Facets and the folders
// plugin's FolderFacet), bulk changes with per-file results (TrashMany),
// and the two storage audits (FindOrphans, MissingObjects).
//
//	go run ./records-query
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
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	diskName = "media"
	tenant   = "acme"
	// ownerPost is the owner type of the demo's posts.
	ownerPost = "post"
	// roleGallery is a post's photo gallery, capped at galleryMax files.
	roleGallery = "gallery"
	galleryMax  = 3
	// roleDocs is a post's documents, not capped.
	roleDocs = "docs"
	// pageSize is the attachment page size; tiny so pagination shows.
	pageSize = 2
)

// pngBytes is enough of a PNG for the disk to detect image/png.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-demo")

// newStore wires a Store with a capped gallery role and the folders
// plugin, and creates its tables.
// ds:def id=newquerystore-a3w0yj
func newStore(ctx context.Context, db *sql.DB, fp *folders.Plugin) (*records.Store, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithOwnerTypes(ownerPost),
		records.WithRoleLimit(roleGallery, galleryMax),
		records.WithPlugins(fp),
	)
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// record stores body under key and records it.
func record(ctx context.Context, s *records.Store, key string, body []byte, contentType string) (records.File, error) {
	disk, err := s.Disks().Disk(diskName)
	if err != nil {
		return records.File{}, err
	}
	if _, err := disk.WriteBytes(ctx, key, body, filesystem.WriteOptions{}); err != nil {
		return records.File{}, err
	}
	return s.CreateFile(ctx, records.NewFile{Scope: tenant, Disk: diskName, Key: key, ContentType: contentType})
}

// attachPhotos attaches photos to the post's capped gallery and returns the
// error of the first one refused.
// ds:def id=attachphotos-y5k2hb
func attachPhotos(ctx context.Context, s *records.Store, post records.Owner, photos []records.File) error {
	for _, f := range photos {
		if _, err := s.Attach(ctx, tenant, f.ID, post, roleGallery); err != nil {
			return fmt.Errorf("photo %s: %w", f.Key, err)
		}
	}
	return nil
}

// postPDFs lists the post's PDFs in any role, newest first: the owner
// filter combined with a content-type condition, paged like any listing.
// ds:def id=postpdfs-c8m1qe
func postPDFs(ctx context.Context, s *records.Store, post records.Owner) (records.FilePage, error) {
	attached, err := s.AttachedTo(tenant, post, "")
	if err != nil {
		return records.FilePage{}, err
	}
	return s.Files(ctx, records.FileQuery{
		Scope:   tenant,
		Filters: []sqlb.Predicate{attached, sqlb.Eq(records.FileContentType, "application/pdf")},
	})
}

// galleryPages walks the post's gallery a page at a time.
// ds:def id=gallerypages-q7d5vz
func galleryPages(ctx context.Context, s *records.Store, post records.Owner) ([][]string, error) {
	var pages [][]string
	cursor := ""
	for {
		page, err := s.AttachmentsPage(ctx, records.AttachmentQuery{
			Scope: tenant, Owner: post, Role: roleGallery, Limit: pageSize, Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		var keys []string
		for _, af := range page.Files {
			keys = append(keys, af.File.Key)
		}
		pages = append(pages, keys)
		if page.NextCursor == "" {
			return pages, nil
		}
		cursor = page.NextCursor
	}
}

// sidebar renders the facet counts of the tenant's live files: by content
// type, by visibility and by folder.
// ds:def id=sidebar-n4r8tk
func sidebar(ctx context.Context, s *records.Store, fp *folders.Plugin) (string, error) {
	fq := records.FileQuery{Scope: tenant}
	var lines []string
	for _, facet := range records.FacetValues {
		counts, err := s.Facets(ctx, fq, facet)
		if err != nil {
			return "", err
		}
		lines = append(lines, string(facet)+": "+render(counts, nil))
	}
	byFolder, err := fp.FolderFacet(ctx, s, fq)
	if err != nil {
		return "", err
	}
	names, err := folderNames(ctx, s, fp)
	if err != nil {
		return "", err
	}
	lines = append(lines, "folder: "+render(byFolder, names))
	return strings.Join(lines, "\n"), nil
}

// folderNames maps the tenant's top-level folder IDs, and the root, to
// names for display.
func folderNames(ctx context.Context, s *records.Store, fp *folders.Plugin) (map[string]string, error) {
	top, err := fp.Children(ctx, s, tenant, folders.RootID)
	if err != nil {
		return nil, err
	}
	names := map[string]string{folders.RootID: "(root)"}
	for _, f := range top {
		names[f.ID] = f.Name
	}
	return names, nil
}

func render(counts []records.FacetCount, names map[string]string) string {
	var out []string
	for _, c := range counts {
		v := c.Value
		if names != nil {
			v = names[v]
		}
		out = append(out, fmt.Sprintf("%s %d (%d B)", v, c.Files, c.Bytes))
	}
	return strings.Join(out, ", ")
}

// trashSelection trashes the files a user selected, each on its own, and
// reports which went through: one bad ID does not undo the rest.
// ds:def id=trashselection-h2p6xw
func trashSelection(ctx context.Context, s *records.Store, ids []string) (string, error) {
	results, err := s.TrashMany(ctx, tenant, ids)
	if err != nil {
		return "", err
	}
	var out []string
	for _, r := range results {
		status := "ok"
		if r.Err != nil {
			status = string(filesystem.CodeOf(r.Err))
		}
		out = append(out, status)
	}
	return strings.Join(out, ", "), nil
}

// storageAudit lists, without changing anything, the objects a sweep
// would delete and the rows whose object is gone.
// ds:def id=storageaudit-u9f3ld
func storageAudit(ctx context.Context, s *records.Store) (orphans, missing []string, err error) {
	_, err = s.FindOrphans(ctx, diskName, "", time.Now().Add(time.Minute), func(key string) {
		orphans = append(orphans, key)
	})
	if err != nil {
		return nil, nil, err
	}
	_, err = s.MissingObjects(ctx, diskName, func(f records.File) {
		missing = append(missing, f.Key)
	})
	return orphans, missing, err
}

func run(ctx context.Context, w io.Writer, dir string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "records.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	fp := folders.New()
	s, err := newStore(ctx, db, fp)
	if err != nil {
		return err
	}
	post := records.Owner{Type: ownerPost, ID: "p1"}

	var photos []records.File
	for i := 1; i <= galleryMax+1; i++ {
		f, err := record(ctx, s, fmt.Sprintf("photos/%d.png", i), pngBytes, "")
		if err != nil {
			return err
		}
		photos = append(photos, f)
	}
	err = attachPhotos(ctx, s, post, photos)
	fmt.Fprintf(w, "attach %d photos to a gallery of %d: %s\n", len(photos), galleryMax, filesystem.CodeOf(err))

	report, err := record(ctx, s, "docs/report.pdf", []byte("%PDF-1.7 report"), "application/pdf")
	if err != nil {
		return err
	}
	if _, err := s.Attach(ctx, tenant, report.ID, post, roleDocs); err != nil {
		return err
	}
	if _, err := record(ctx, s, "docs/other.pdf", []byte("%PDF-1.7 other"), "application/pdf"); err != nil {
		return err
	}
	pdfs, err := postPDFs(ctx, s, post)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "the post's PDFs: %d (%s)\n", len(pdfs.Files), pdfs.Files[0].Key)

	pages, err := galleryPages(ctx, s, post)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "gallery pages: %v\n", pages)

	trip, err := fp.Create(ctx, s, tenant, folders.RootID, "Trip")
	if err != nil {
		return err
	}
	for _, f := range photos[:2] {
		if err := fp.Put(ctx, s, tenant, f.ID, trip.ID); err != nil {
			return err
		}
	}
	if err := s.SetVisibility(ctx, tenant, photos[0].ID, filesystem.VisibilityPublic); err != nil {
		return err
	}
	bar, err := sidebar(ctx, s, fp)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, bar)

	trashed, err := trashSelection(ctx, s, []string{photos[3].ID, "fil_unknown", report.ID})
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "trash 3 selected: %s\n", trashed)

	disk, err := s.Disks().Disk(diskName)
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "tmp/left-behind.bin", []byte("x"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	if err := disk.Delete(ctx, "docs/other.pdf"); err != nil {
		return err
	}
	orphans, missing, err := storageAudit(ctx, s)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "orphans: %v; rows without an object: %v\n", orphans, missing)
	return nil
}

func main() {
	dir, err := os.MkdirTemp("", "records-query")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
