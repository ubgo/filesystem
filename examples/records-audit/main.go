// Command records-audit shows the audit plugin of the records Store on
// SQLite and an in-memory disk: installing it, naming the actor of a
// request, producing every kind of event, reading a file's history and
// an actor's activity page by page, the rollback guarantee, and pruning
// old entries.
//
//	go run ./records-audit
//
// It prints what each step returned; main_test.go runs the same steps.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
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
	"github.com/ubgo/filesystem/records/audit"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	// diskName is the one disk files are stored on.
	diskName = "media"
	// tenant is the scope every file of the demo belongs to.
	tenant = "acme"
	// ownerPost is the one owner type files are attached to.
	ownerPost = "post"
	// roleGallery is the role the demo attaches files under.
	roleGallery = "gallery"
	// postID is the post the demo attaches files to.
	postID = "42"
	// actorAlice and actorCron are the actors the demo acts as: a signed-in
	// user and a background job.
	actorAlice = "user:alice"
	actorCron  = "system:cron"
	// maxUpload caps one upload.
	maxUpload = 1 << 20
	// activityPage is the page size of the activity feed; tiny so
	// pagination shows.
	activityPage = 3
	// retention is how long entries are kept before Prune deletes them.
	retention = 90 * 24 * time.Hour
)

// errDemoRollback aborts the transaction of the rollback demo.
var errDemoRollback = errors.New("demo: request failed after the upload")

// textPolicy accepts plain text with a random key under docs/.
var textPolicy = upload.Policy{MaxSize: maxUpload, Allowed: []string{"text/plain"}, Key: upload.RandomKey("docs")}

// archiveDisk is a second disk files can move to (MoveFile).
const archiveDisk = "archive"

// newDisks builds the manager the Store reaches bytes through: two memory
// disks, which support SetVisibility.
func newDisks() (*filesystem.Manager, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, err
	}
	archive, err := filesystem.NewDisk(archiveDisk, memory.New())
	if err != nil {
		return nil, err
	}
	return filesystem.NewManager(disk, archive)
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

// newStore installs the audit plugin and migrates, which creates fs_audit
// next to the core tables.
// ds:def id=newstore-4az2gwj7
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager, au *audit.Plugin) (*records.Store, error) {
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithOwnerTypes(ownerPost), records.WithPlugins(au))
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// sessionUserKey is where this app's own auth middleware keeps the
// signed-in user's ID.
type sessionUserKey struct{}

// newPluginWithOwnKey reads the actor from the app's own context key
// instead of records.WithActor, for apps that already store the user.
// ds:def id=newpluginwithownkey-ss59fp5h
func newPluginWithOwnKey() *audit.Plugin {
	return audit.New(audit.WithActorFunc(func(ctx context.Context) string {
		if id, ok := ctx.Value(sessionUserKey{}).(string); ok {
			return "user:" + id
		}
		return ""
	}))
}

// everyChange makes one change of every event kind as actor, and returns
// the file it worked on.
// ds:def id=everychange-aq6m25az
func everyChange(ctx context.Context, s *records.Store, actor string) (records.File, error) {
	ctx = audit.WithActor(ctx, actor)
	post := records.Owner{Type: ownerPost, ID: postID}
	f, err := s.Upload(ctx, tenant, diskName, strings.NewReader("draft one"), "notes.txt", textPolicy) // file.created
	if err != nil {
		return records.File{}, err
	}
	a, err := s.Attach(ctx, tenant, f.ID, post, roleGallery) // attachment.created
	if err != nil {
		return records.File{}, err
	}
	if err := s.Reorder(ctx, tenant, post, roleGallery, []string{a.ID}); err != nil { // attachments.reordered
		return records.File{}, err
	}
	if _, err := s.Replace(ctx, tenant, f.ID, strings.NewReader("draft two"), "", textPolicy); err != nil { // file.replaced
		return records.File{}, err
	}
	if _, err := s.SetStatus(ctx, tenant, f.ID, records.StatusQuarantined, "rescan"); err != nil { // file.status_changed
		return records.File{}, err
	}
	if _, err := s.PassScan(ctx, tenant, f.ID); err != nil { // file.status_changed, file.released
		return records.File{}, err
	}
	if err := s.SetVisibility(ctx, tenant, f.ID, filesystem.VisibilityPublic); err != nil { // file.visibility_changed
		return records.File{}, err
	}
	if _, err := s.MoveFile(ctx, tenant, f.ID, archiveDisk); err != nil { // file.moved
		return records.File{}, err
	}
	if err := s.Detach(ctx, tenant, a.ID); err != nil { // attachment.deleted
		return records.File{}, err
	}
	if err := s.Trash(ctx, tenant, f.ID); err != nil { // file.trashed
		return records.File{}, err
	}
	if err := s.Restore(ctx, tenant, f.ID); err != nil { // file.restored
		return records.File{}, err
	}
	return f, s.Purge(ctx, tenant, f.ID) // file.purged
}

// history is a file's log, newest first: what the file page shows.
// ds:def id=history-23qsz95p
func history(ctx context.Context, s *records.Store, au *audit.Plugin, fileID string) ([]audit.Entry, error) {
	page, err := au.Entries(ctx, s, audit.Query{Scope: tenant, FileID: fileID})
	return page.Entries, err
}

// activity walks an actor's whole feed page by page, following
// NextCursor, and returns the entries and how many pages it took.
// ds:def id=activity-n7j5jaye
func activity(ctx context.Context, s *records.Store, au *audit.Plugin, actor string) ([]audit.Entry, int, error) {
	var all []audit.Entry
	q := audit.Query{Scope: tenant, Actor: actor, Limit: activityPage}
	for pages := 1; ; pages++ {
		page, err := au.Entries(ctx, s, q)
		if err != nil {
			return nil, pages, err
		}
		all = append(all, page.Entries...)
		if page.NextCursor == "" {
			return all, pages, nil
		}
		q.Cursor = page.NextCursor
	}
}

// rolledBack uploads inside a transaction that then fails: the file, its
// object and its audit entry all go, so the log never shows a change
// that did not happen.
// ds:def id=rolledback-df6ptbdc
func rolledBack(ctx context.Context, s *records.Store) error {
	ctx = audit.WithActor(ctx, actorAlice)
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := s.Upload(ctx, tenant, diskName, strings.NewReader("lost"), "lost.txt", textPolicy); err != nil {
			return err
		}
		return errDemoRollback
	})
}

// tooLongActor tries a change with an actor past MaxActorBytes: the
// change is refused rather than logged with a truncated actor.
// ds:def id=toolongactor-vjp5uu4w
func tooLongActor(ctx context.Context, s *records.Store) error {
	ctx = audit.WithActor(ctx, "user:"+strings.Repeat("x", audit.MaxActorBytes))
	_, err := s.Upload(ctx, tenant, diskName, bytes.NewReader([]byte("hi")), "hi.txt", textPolicy)
	return err
}

// prune applies the retention period; schedule it (the library starts no
// background work).
// ds:def id=prune-vhnhy5ef
func prune(ctx context.Context, s *records.Store, au *audit.Plugin, now time.Time) (int64, error) {
	return au.Prune(ctx, s, now.Add(-retention))
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
	au := audit.New()
	s, err := newStore(ctx, db, disks, au)
	if err != nil {
		return err
	}
	if err := demo(ctx, w, s, au); err != nil {
		return err
	}
	if err := chainDemo(ctx, w, db, disks); err != nil {
		return err
	}
	return rotateDemo(ctx, w, dir)
}

// demo is run's body once the Store exists.
func demo(ctx context.Context, w io.Writer, s *records.Store, au *audit.Plugin) error {
	f, err := everyChange(ctx, s, actorAlice)
	if err != nil {
		return err
	}
	if _, err := s.Upload(audit.WithActor(ctx, actorCron), tenant, diskName, strings.NewReader("report"), "nightly.txt", textPolicy); err != nil {
		return err
	}
	entries, err := history(ctx, s, au, f.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "history of the purged file (%d entries, newest first):\n", len(entries))
	for _, e := range entries {
		line := fmt.Sprintf("  %-24s actor=%s owner=%s/%s role=%s", e.Kind, e.Actor, e.Owner.Type, e.Owner.ID, e.Role)
		if len(e.Detail) > 0 {
			line += fmt.Sprintf(" from=%s to=%s", e.Detail[records.DetailFrom], e.Detail[records.DetailTo])
		}
		fmt.Fprintln(w, line)
	}
	feed, pages, err := activity(ctx, s, au, actorAlice)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "activity of %s: %d entries in %d pages\n", actorAlice, len(feed), pages)
	fmt.Fprintf(w, "rollback: %v\n", rolledBack(ctx, s))
	feed, _, err = activity(ctx, s, au, actorAlice)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "activity after the rollback: %d entries\n", len(feed))
	fmt.Fprintf(w, "long actor: %s\n", filesystem.CodeOf(tooLongActor(ctx, s)))
	n, err := prune(ctx, s, au, time.Now().Add(retention+time.Hour))
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "pruned %d entries\n", n)
	return nil
}

func main() {
	dir, err := os.MkdirTemp("", "records-audit")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
