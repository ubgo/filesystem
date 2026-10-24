// Command records-quota shows the quota plugin of the records Store:
// limits per scope taken from the scope's plan, uploads refused past a
// limit, usage that counts trashed files and releases purged ones, a
// replacement charged by its size difference, a rolled-back upload that
// un-charges itself, and Recount after a manual repair.
//
//	go run ./records-quota
//
// It prints the usage after every step; main_test.go runs the same steps.
package main

import (
	"context"
	"database/sql"
	"errors"
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
	// diskName is the one disk files are stored on.
	diskName = "media"
	// freeTenant is a scope on the free plan.
	freeTenant = "acme"
	// proTenant is a scope on the pro plan.
	proTenant = "globex"
	// maxUpload caps one upload.
	maxUpload = 1 << 20
	// maxRetries bounds the retries of an upload that lost the first-upload
	// race of its scope.
	maxRetries = 3
)

// Plans of the demo app: what each plan may store. Zero is unlimited.
var (
	freePlan = quota.Limit{Bytes: 25, Files: 3}
	proPlan  = quota.Limit{Bytes: 1 << 30}
)

// planOf is the app's lookup of a scope's plan; a real app reads its
// billing table here.
var planOf = map[string]quota.Limit{freeTenant: freePlan, proTenant: proPlan, inflightTenant: freePlan}

// inflightTenant is the free-plan scope of the reservations demo
// (inflight.go), apart so the first half's usage does not show.
const inflightTenant = "free-inflight"

// limits is the quota.LimitFunc: it returns the limit of a scope's plan,
// and an error for a scope the app does not know.
// ds:def id=limits-du27j4uv
func limits(_ context.Context, scope string) (quota.Limit, error) {
	l, ok := planOf[scope]
	if !ok {
		return quota.Limit{}, filesystem.Errorf(filesystem.CodePermission, "no plan for scope %q", scope)
	}
	return l, nil
}

// textPolicy accepts plain text only, with a random key under files/.
var textPolicy = upload.Policy{MaxSize: maxUpload, Allowed: []string{"text/plain"}, Key: upload.RandomKey("files")}

// newDisks builds the manager the Store reaches bytes through: one
// memory disk.
func newDisks() (*filesystem.Manager, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, err
	}
	return filesystem.NewManager(disk)
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

// newStore installs the quota plugin, and the versions plugin to show
// that kept versions are not charged, then creates every table.
// ds:def id=newstore-cnhfwgs6
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, *quota.Plugin, error) {
	q, err := quota.New(limits)
	if err != nil {
		return nil, nil, err
	}
	v, err := versions.New()
	if err != nil {
		return nil, nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(q, v))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, q, nil
}

// uploadText stores body as a file of scope. The quota plugin charges it
// inside the same transaction, or refuses it with CodeQuotaExceeded.
// ds:def id=uploadtext-8afxkytn
func uploadText(ctx context.Context, s *records.Store, scope, body string) (records.File, error) {
	return s.Upload(ctx, scope, diskName, strings.NewReader(body), "note.txt", textPolicy)
}

// uploadWithRetry retries an upload that failed with CodeUnavailable: two
// first uploads of one scope race to create its usage row, and the loser
// fails that way. A later attempt finds the row and is charged normally.
// ds:def id=uploadwithretry-g656j6mk
func uploadWithRetry(ctx context.Context, s *records.Store, scope, body string) (records.File, error) {
	var err error
	for range maxRetries {
		var f records.File
		if f, err = uploadText(ctx, s, scope, body); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
			return f, err
		}
	}
	return records.File{}, err
}

// usageLine formats what a scope stores now, for a billing page.
// ds:def id=usageline-m7guc4ar
func usageLine(ctx context.Context, s *records.Store, q *quota.Plugin, scope string) (string, error) {
	u, err := q.Usage(ctx, s, scope)
	if err != nil {
		return "", err
	}
	l, err := limits(ctx, scope)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s: %d of %d bytes, %d of %d files", scope, u.Bytes, l.Bytes, u.Files, l.Files), nil
}

// errAbort stands for any failure later in a request that uploaded.
var errAbort = errors.New("request failed after the upload")

// uploadThenFail uploads inside a transaction that then fails: the row,
// the object and the quota charge all go with the rollback.
// ds:def id=uploadthenfail-9vecxbsh
func uploadThenFail(ctx context.Context, s *records.Store, scope string) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := uploadText(ctx, s, scope, "draft"); err != nil {
			return err
		}
		return errAbort
	})
}

// replaceContent gives a file new content. The plugin charges the size
// difference: growth past the byte limit is refused, a shrink is
// credited. The replaced content is kept by the versions plugin, and is
// not charged.
// ds:def id=replacecontent-v8pf85nf
func replaceContent(ctx context.Context, s *records.Store, f records.File, body string) (records.File, error) {
	return s.Replace(ctx, f.Scope, f.ID, strings.NewReader(body), "", textPolicy)
}

// repair rebuilds a scope's usage from fs_files, after rows were changed
// by hand or the plugin was installed after the scope's first files.
// ds:def id=repair-828jum3m
func repair(ctx context.Context, s *records.Store, q *quota.Plugin, scope string) (quota.Usage, error) {
	return q.Recount(ctx, s, scope)
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
	s, q, err := newStore(ctx, db, disks)
	if err != nil {
		return err
	}
	if err := demo(ctx, w, s, q); err != nil {
		return err
	}
	if err := inflight(ctx, w, db, disks); err != nil {
		return err
	}
	return orgCounters(ctx, w, dir)
}

// demo is run's body once the Store exists.
func demo(ctx context.Context, w io.Writer, s *records.Store, q *quota.Plugin) error {
	show := func(step string) error {
		line, err := usageLine(ctx, s, q, freeTenant)
		fmt.Fprintf(w, "%-22s %s\n", step+":", line)
		return err
	}
	a, err := uploadWithRetry(ctx, s, freeTenant, "0123456789")
	if err != nil {
		return err
	}
	if _, err := uploadWithRetry(ctx, s, freeTenant, "abcdefgh"); err != nil {
		return err
	}
	if err := show("two uploads"); err != nil {
		return err
	}
	_, err = uploadText(ctx, s, freeTenant, "this one is too big")
	fmt.Fprintf(w, "over the byte limit:   %s\n", filesystem.CodeOf(err))
	if err := uploadThenFail(ctx, s, freeTenant); !errors.Is(err, errAbort) {
		return fmt.Errorf("rollback demo: %w", err)
	}
	if err := show("after a rollback"); err != nil {
		return err
	}
	return lifecycle(ctx, w, s, q, a, show)
}

// lifecycle shows replace, trash, purge and recount on file a.
func lifecycle(ctx context.Context, w io.Writer, s *records.Store, q *quota.Plugin, a records.File, show func(string) error) error {
	if _, err := replaceContent(ctx, s, a, "0123"); err != nil {
		return err
	}
	if err := show("replaced 10 -> 4"); err != nil {
		return err
	}
	_, err := replaceContent(ctx, s, a, "01234567890123456789")
	fmt.Fprintf(w, "growth past the limit: %s\n", filesystem.CodeOf(err))
	if err := s.Trash(ctx, freeTenant, a.ID); err != nil {
		return err
	}
	if err := show("trashed"); err != nil {
		return err
	}
	if err := s.Purge(ctx, freeTenant, a.ID); err != nil {
		return err
	}
	if err := show("purged"); err != nil {
		return err
	}
	u, err := repair(ctx, s, q, freeTenant)
	fmt.Fprintf(w, "recount:               %d bytes, %d files\n", u.Bytes, u.Files)
	return err
}

func main() {
	dir, err := os.MkdirTemp("", "records-quota")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
