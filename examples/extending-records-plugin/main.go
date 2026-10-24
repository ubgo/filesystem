// Command extending-records-plugin writes a records plugin of its own:
// legal holds. A held file cannot be trashed or purged until the hold is
// released. The plugin has its own table and migration, takes part in the
// file lifecycle through FileHooks, vetoes changes through EventHooks,
// joins the caller's transaction for its own writes, and runs a
// notification only after the transaction commits.
//
//	go run ./extending-records-plugin
//
// A real plugin lives in a package of its own; it is in package main here
// so the example is one runnable file.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// PluginName names the plugin in the migration ledger and in errors. It
// must be unique among installed plugins and match [a-z][a-z0-9_.]*.
const PluginName = "legalhold"

// TableNameHolds is the table the plugin owns. Prefixing it fs_ keeps it
// next to the library's tables.
const TableNameHolds = "fs_legal_holds"

// Column names of fs_legal_holds.
const (
	ColScope  = "scope"
	ColFileID = "file_id"
	ColReason = "reason"
	ColHeldBy = "held_by"
	ColHeldAt = "held_at"
)

// Table and typed columns, for sqlb statements.
var (
	Table  = sqlb.Table{Name: TableNameHolds, Alias: "lh"}
	Scope  = sqlb.Col[string](Table, ColScope)
	FileID = sqlb.Col[string](Table, ColFileID)
	Reason = sqlb.Col[string](Table, ColReason)
	HeldBy = sqlb.Col[string](Table, ColHeldBy)
	HeldAt = sqlb.Col[time.Time](Table, ColHeldAt)
)

// Hold is one file under legal hold.
type Hold struct {
	Scope  string
	FileID string
	Reason string
	HeldBy string
	HeldAt time.Time
}

// Plugin manages legal holds. Install it with records.WithPlugins.
type Plugin struct {
	notify func(context.Context, Hold)
}

// Option configures a Plugin.
type Option func(*Plugin)

// WithNotify sets a function told of every new hold once it has
// committed, for a compliance mailbox or a webhook. It never runs for a
// hold whose transaction rolled back.
func WithNotify(fn func(context.Context, Hold)) Option {
	return func(p *Plugin) { p.notify = fn }
}

// New returns a Plugin.
func New(opts ...Option) *Plugin {
	p := &Plugin{notify: func(context.Context, Hold) {}}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Name implements records.Plugin, the one required method.
// ds:def id=plugin-name-2xqe7aq8
func (p *Plugin) Name() string { return PluginName }

// Migrations implements records.Migrator. The table is built with
// records.DDL from the same typed columns the plugin's queries use, spelled
// for the engine by records.TypesFor (the types the library's own tables
// use), so one definition serves SQLite, Postgres and MySQL and every
// statement can run again after a crash. The migration's Name is
// permanent: changing its statements after it ran makes Migrate fail
// rather than silently skip or re-run it, so a schema change is a new
// migration (records.DDL's AddColumn adds a column later).
// ds:def id=plugin-migrations-e4sm8uz8
func (p *Plugin) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	st, err := records.NewDDL(engine).CreateTable(records.Table{
		Table: Table,
		Columns: []ddl.ColumnSpec{
			records.Column(Scope, t.Key), records.Column(FileID, t.Key), records.Column(Reason, t.Text),
			records.Column(HeldBy, t.Key), records.Column(HeldAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{Scope, FileID},
	}).Statements()
	return []records.Migration{{Owner: PluginName, Name: "0001_legal_holds", Statements: st}}, err
}

// errHeld is the cause of every refusal of a held file.
var errHeld = errors.New("the file is under legal hold")

// isHeld reports whether a file is held. It runs on Store.Conn(ctx), so
// inside a hook it reads within the operation's transaction.
// ds:def id=plugin-isheld-cews2uet
func (p *Plugin) isHeld(ctx context.Context, s *records.Store, scope, fileID string) (bool, error) {
	var id string
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(FileID, &id)).From(Table).
		Where(sqlb.Eq(Scope, scope), sqlb.Eq(FileID, fileID))
	err := s.One(ctx, q)
	if errors.Is(err, sqlb.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Hold places a file under hold, recording who held it (the actor set with
// records.WithActor). A file in the trash must be restored first: a held
// file that is already trashed would make every SweepTrash run report its
// refusal (the sweep purges the other files and names the held one in its
// error) until the hold is released.
// ds:def id=plugin-hold-mg4j96bw
func (p *Plugin) Hold(ctx context.Context, s *records.Store, scope, fileID, reason string) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		f, err := s.File(ctx, scope, fileID)
		if err != nil {
			return err
		}
		if f.DeletedAt != nil {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "%s: restore file %s before holding it", PluginName, fileID)
		}
		h := Hold{Scope: scope, FileID: fileID, Reason: reason, HeldBy: s.Actor(ctx), HeldAt: time.Now().UTC().Truncate(time.Microsecond)}
		ins := sqlb.NewInsert(s.Dialect()).Into(Table).Row(sqlb.Set(Scope, h.Scope), sqlb.Set(FileID, h.FileID),
			sqlb.Set(Reason, h.Reason), sqlb.Set(HeldBy, h.HeldBy), sqlb.Set(HeldAt, h.HeldAt))
		if _, err := s.Exec(ctx, ins); err != nil {
			if records.IsUniqueViolation(err) {
				return filesystem.Errorf(filesystem.CodeExists, "%s: file %s is already held", PluginName, fileID)
			}
			return err
		}
		s.AfterCommit(ctx, func(ctx context.Context) { p.notify(ctx, h) })
		return nil
	})
}

// Release lifts a hold; CodeNotFound when there is none.
// ds:def id=plugin-release-ce86gtzx
func (p *Plugin) Release(ctx context.Context, s *records.Store, scope, fileID string) error {
	del := sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Scope, scope), sqlb.Eq(FileID, fileID))
	n, err := s.Exec(ctx, del)
	if err != nil {
		return err
	}
	if n == 0 {
		return filesystem.Errorf(filesystem.CodeNotFound, "%s: file %s is not held", PluginName, fileID)
	}
	return nil
}

// BeforeCreate implements records.FileHooks; holds do not affect uploads.
func (p *Plugin) BeforeCreate(context.Context, *records.Store, *records.File) error { return nil }

// AfterCreate implements records.FileHooks.
func (p *Plugin) AfterCreate(context.Context, *records.Store, records.File) error { return nil }

// BeforePurge implements records.FileHooks: a held file is never purged,
// whether by Purge, PurgeOwner or SweepTrash. The error rolls the purge
// back before any row or object is deleted.
// ds:def id=plugin-beforepurge-az88amcp
func (p *Plugin) BeforePurge(ctx context.Context, s *records.Store, f records.File) error {
	held, err := p.isHeld(ctx, s, f.Scope, f.ID)
	if err != nil {
		return err
	}
	if held {
		return filesystem.NewError(filesystem.CodePermission, errHeld)
	}
	return nil
}

// OnEvent implements records.EventHooks. FileHooks has no hook before a
// trash, but every change emits an event inside its own transaction, and an
// error from OnEvent rolls the change back: here, trashing a held file.
// ds:def id=plugin-onevent-7x79zumk
func (p *Plugin) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	if e.Kind != records.EventFileTrashed {
		return nil
	}
	held, err := p.isHeld(ctx, s, e.Scope, e.FileID)
	if err != nil {
		return err
	}
	if held {
		return filesystem.NewError(filesystem.CodePermission, errHeld)
	}
	return nil
}

// Settings of the demo.
const (
	scope    = "acme"
	diskName = "media"
	actor    = "counsel@acme.test"
)

// docs is the upload policy of the demo.
var docs = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"text/plain"}, Key: upload.RandomKey("docs")}

// newStore opens SQLite in memory with the plugin installed and migrates
// it; Migrate applies the core's migrations and then the plugin's.
// ds:def id=newstore-ubxccq5d
func newStore(ctx context.Context, p *Plugin) (*records.Store, []string, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(p))
	if err != nil {
		return nil, nil, err
	}
	applied, err := store.Migrate(ctx)
	return store, applied, err
}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run holds a file, shows trash and purge refused, releases it, and
// sweeps the trash.
func run(ctx context.Context, out io.Writer) error {
	p := New(WithNotify(func(_ context.Context, h Hold) {
		fmt.Fprintf(out, "notified: %s held %s (%s)\n", h.HeldBy, h.FileID, h.Reason)
	}))
	store, applied, err := newStore(ctx, p)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "migrations: %s\n", strings.Join(applied, ", "))
	ctx = records.WithActor(ctx, actor)
	f, err := store.Upload(ctx, scope, diskName, strings.NewReader("board minutes\n"), "minutes.txt", docs)
	if err != nil {
		return err
	}
	if err := p.Hold(ctx, store, scope, f.ID, "litigation 2026-17"); err != nil {
		return err
	}
	fmt.Fprintf(out, "trash: %v\n", store.Trash(ctx, scope, f.ID))
	fmt.Fprintf(out, "purge: %v\n", store.Purge(ctx, scope, f.ID))
	if err := p.Release(ctx, store, scope, f.ID); err != nil {
		return err
	}
	if err := store.Trash(ctx, scope, f.ID); err != nil {
		return err
	}
	n, err := store.SweepTrash(ctx, time.Now().Add(time.Minute))
	fmt.Fprintf(out, "after release: trashed and swept %d file(s), %v\n", n, err)
	return nil
}
