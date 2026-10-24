// Command records-tags shows the tags plugin of the records Store on
// SQLite and an in-memory disk: setting, adding and removing a file's
// tags, how tags are normalized, listing files by tag (alone or combined
// with another filter), a tag cloud, and renaming or deleting a tag
// across a scope.
//
//	go run ./records-tags
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
	"github.com/ubgo/filesystem/records/tags"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	// diskName is the one disk files are stored on.
	diskName = "media"
	// tenant is the scope every file and tag of the demo belongs to.
	tenant = "acme"
	// maxUpload caps one upload.
	maxUpload = 1 << 20
	// smallFile is the size bound of the combined filter, in bytes.
	smallFile = 20
)

// textPolicy accepts plain text with a random key.
var textPolicy = upload.Policy{MaxSize: maxUpload, Allowed: []string{"text/plain"}, Key: upload.RandomKey("docs")}

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

// newStore installs the tags plugin and migrates, which creates
// fs_file_tags.
// ds:def id=newstore-uc78dg7p
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager, tp *tags.Plugin) (*records.Store, error) {
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(tp))
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// slug is a Normalize for apps that want URL-safe tags: trimmed,
// lowercased, inner spaces as dashes. It ends with tags.Check, which
// refuses what cannot be stored.
// ds:def id=slug-rvcmc6am
func slug(tag string) (string, error) {
	return tags.Check(strings.Join(strings.Fields(strings.ToLower(tag)), "-"))
}

// newSlugPlugin installs slug in place of tags.DefaultNormalize.
// ds:def id=newslugplugin-pqknt7r8
func newSlugPlugin() *tags.Plugin { return tags.New(tags.WithNormalize(slug)) }

// uploadText uploads one text file to the demo's scope.
func uploadText(ctx context.Context, s *records.Store, name, body string) (records.File, error) {
	return s.Upload(ctx, tenant, diskName, strings.NewReader(body), name, textPolicy)
}

// tagFiles shows Set, Add and Remove; each returns the file's tags as
// stored, sorted.
// ds:def id=tagfiles-7nhadn2v
func tagFiles(ctx context.Context, s *records.Store, tp *tags.Plugin, a, b, c records.File) error {
	if _, err := tp.Set(ctx, s, tenant, a.ID, "Invoice", " 2026 ", "invoice"); err != nil {
		return err
	}
	if _, err := tp.Add(ctx, s, tenant, b.ID, "invoice", "Paid", "draft"); err != nil {
		return err
	}
	if _, err := tp.Remove(ctx, s, tenant, b.ID, "DRAFT", "never-there"); err != nil {
		return err
	}
	_, err := tp.Set(ctx, s, tenant, c.ID, "paid", "receipt")
	return err
}

// filesTagged lists the live files carrying every one of want, with the
// order and pagination of records.Store.Files.
// ds:def id=filestagged-vmpsqbet
func filesTagged(ctx context.Context, s *records.Store, tp *tags.Plugin, want ...string) ([]string, error) {
	page, err := tp.Files(ctx, s, records.FileQuery{Scope: tenant}, want...)
	return filenames(page), err
}

// smallInvoices combines the tag filter with a filter of the app's own:
// both are predicates on fs_files in one FileQuery.
// ds:def id=smallinvoices-q3rq7yyy
func smallInvoices(ctx context.Context, s *records.Store, tp *tags.Plugin) ([]string, error) {
	tagged, err := tp.Filter(s, tenant, "invoice")
	if err != nil {
		return nil, err
	}
	page, err := s.Files(ctx, records.FileQuery{
		Scope:   tenant,
		Filters: []sqlb.Predicate{tagged, sqlb.Less(records.FileSize, int64(smallFile))},
	})
	return filenames(page), err
}

// filenames returns the names of a page's files.
func filenames(page records.FilePage) []string {
	out := make([]string, 0, len(page.Files))
	for _, f := range page.Files {
		out = append(out, f.Filename)
	}
	return out
}

// cloud renders the tag cloud: every tag with its live file count, most
// used first.
// ds:def id=cloud-85yyz6vr
func cloud(ctx context.Context, s *records.Store, tp *tags.Plugin) (string, error) {
	counts, err := tp.Counts(ctx, s, tenant)
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(counts))
	for _, c := range counts {
		parts = append(parts, fmt.Sprintf("%s(%d)", c.Tag, c.Files))
	}
	return strings.Join(parts, " "), nil
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
	tp := tags.New()
	s, err := newStore(ctx, db, disks, tp)
	if err != nil {
		return err
	}
	return demo(ctx, w, s, tp)
}

// demo is run's body once the Store exists.
func demo(ctx context.Context, w io.Writer, s *records.Store, tp *tags.Plugin) error {
	var files []records.File
	for _, n := range []struct{ name, body string }{{"jan.txt", "invoice for january, long"}, {"feb.txt", "feb invoice"}, {"shop.txt", "receipt"}} {
		f, err := uploadText(ctx, s, n.name, n.body)
		if err != nil {
			return err
		}
		files = append(files, f)
	}
	if err := tagFiles(ctx, s, tp, files[0], files[1], files[2]); err != nil {
		return err
	}
	for _, f := range files {
		got, err := tp.Tags(ctx, s, tenant, f.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%s: %v\n", f.Filename, got)
	}
	return demoQueries(ctx, w, s, tp, files)
}

// demoQueries is the second half of demo: listings, the cloud, and
// scope-wide renames and deletes.
// ds:def id=demoqueries-4yrh6m7s
func demoQueries(ctx context.Context, w io.Writer, s *records.Store, tp *tags.Plugin, files []records.File) error {
	both, err := filesTagged(ctx, s, tp, "Invoice", "paid")
	if err != nil {
		return err
	}
	small, err := smallInvoices(ctx, s, tp)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "invoice+paid: %v; invoices under %d bytes: %v\n", both, smallFile, small)
	if err := s.Trash(ctx, tenant, files[2].ID); err != nil {
		return err
	}
	c, err := cloud(ctx, s, tp)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "cloud with shop.txt trashed:", c)
	if err := tp.Rename(ctx, s, tenant, "paid", "invoice"); err != nil {
		return err
	}
	if err := tp.Delete(ctx, s, tenant, "2026"); err != nil {
		return err
	}
	if c, err = cloud(ctx, s, tp); err != nil {
		return err
	}
	fmt.Fprintln(w, "cloud after renaming paid to invoice and deleting 2026:", c)
	return nil
}

func main() {
	dir, err := os.MkdirTemp("", "records-tags")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
