package records

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// Engine names, as sqlb reports them.
const (
	EnginePostgres = sqlb.DialectPostgres
	EngineMySQL    = sqlb.DialectMySQL
	EngineSQLite   = sqlb.DialectSQLite
)

// CoreOwner owns the core migrations in the ledger; no plugin may use it.
const CoreOwner = "records"

// Migration is one named schema change owned by the core or a plugin. Name
// identifies it for ever; changing its Statements after it ran makes the
// next Migrate fail with CodeInvalidArgument rather than skip or re-run it.
//
// The ledger is this package's own rather than sqlb's contrib/migrate,
// which cannot be imported until sqlb is tagged (its go.mod requires an
// untagged v0.1.0); ubgo/auth carries one for the same reason.
type Migration struct {
	Owner      string
	Name       string
	Statements []string
}

// Checksum identifies the migration's statements.
func (m Migration) Checksum() string {
	h := sha256.New()
	for _, st := range m.Statements {
		h.Write([]byte(st))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func engineOf(d sqlb.Dialect) string { return sqlb.DialectName(d) }

func supported(engine string) error {
	switch engine {
	case EnginePostgres, EngineMySQL, EngineSQLite:
		return nil
	}
	return filesystem.Errorf(filesystem.CodeUnsupported, "records: engine %q is not supported", engine)
}

// SQLTypes are each engine's spellings of the types fs_ tables use, so a
// plugin writing its own migration uses the same ones.
type SQLTypes struct {
	// Key is an ID or scope: indexed, up to 191 characters (MySQL's indexed
	// utf8mb4 width).
	Key string
	// Short is an owner type or role: indexed, up to 64 characters, so a
	// composite unique index stays under MySQL's 3072-byte limit.
	Short string
	// Text is a display string up to 255 characters.
	Text string
	// Long is up to 1024 characters (a storage key), not indexed.
	Long string
	// Int is a 64-bit integer.
	Int string
	// Timestamp is a UTC instant with microsecond precision.
	Timestamp string
	// Doc is large text such as JSON, not indexed.
	Doc string
}

// TypesFor returns the spellings for a supported engine.
func TypesFor(engine string) SQLTypes {
	switch engine {
	case EngineMySQL:
		return SQLTypes{Key: "VARCHAR(191)", Short: "VARCHAR(64)", Text: "VARCHAR(255)", Long: "VARCHAR(1024)", Int: "BIGINT", Timestamp: "DATETIME(6)", Doc: "MEDIUMTEXT"}
	case EngineSQLite:
		return SQLTypes{Key: "TEXT", Short: "TEXT", Text: "TEXT", Long: "TEXT", Int: "INTEGER", Timestamp: "TIMESTAMP", Doc: "TEXT"}
	}
	return SQLTypes{Key: "TEXT", Short: "TEXT", Text: "TEXT", Long: "TEXT", Int: "BIGINT", Timestamp: "TIMESTAMPTZ", Doc: "TEXT"}
}

// objectKeyPrefix is how many characters of storage_key MySQL indexes in
// fs_files_object: 255 utf8mb4 characters are 1020 bytes, which with the
// disk (256 bytes) stays under MySQL's 3072-byte key limit.
const objectKeyPrefix = 255

// coreMigrations creates the fs_ tables for a supported engine, built
// with sqlb/ddl from the typed columns the queries use (see DDL).
func coreMigrations(engine string) ([]Migration, error) {
	t := TypesFor(engine)
	tables, err1 := NewDDL(engine).CreateTable(Table{
		Table: TableFiles,
		Columns: []ddl.ColumnSpec{
			Column(FileID, t.Key), Column(FileScope, t.Key), Column(FileDisk, t.Short), Column(FileKey, t.Long),
			Column(FileFilename, t.Text), Column(FileContentType, t.Text), Column(FileSize, t.Int), Column(FileSHA256, t.Short),
			Column(FileVisibility, t.Short), Column(FileMetadata, t.Doc), Column(FileCreatedAt, t.Timestamp), Column(FileDeletedAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{FileID},
		Indexes: []Index{
			{Name: "fs_files_scope_created", Columns: []IndexColumn{Key(FileScope), Key(FileCreatedAt), Key(FileID)}},
			{Name: "fs_files_scope_sha256", Columns: []IndexColumn{Key(FileScope), Key(FileSHA256)}},
		},
	}).CreateTable(Table{
		Table: TableAttachments,
		Columns: []ddl.ColumnSpec{
			Column(AttachmentID, t.Key), Column(AttachmentScope, t.Key), Column(AttachmentFileID, t.Key), Column(AttachmentOwnerType, t.Short),
			Column(AttachmentOwnerID, t.Key), Column(AttachmentRole, t.Short), Column(AttachmentPosition, t.Int), Column(AttachmentCreatedAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{AttachmentID},
		Indexes: []Index{
			{Name: "fs_attachments_owner", Unique: true, Columns: []IndexColumn{
				Key(AttachmentScope), Key(AttachmentOwnerType), Key(AttachmentOwnerID), Key(AttachmentRole), Key(AttachmentFileID),
			}},
			{Name: "fs_attachments_file", Columns: []IndexColumn{Key(AttachmentFileID)}},
		},
	}).Statements()
	uploadedBy, err2 := NewDDL(engine).AddColumn(TableFiles, Column(FileUploadedBy, t.Key).Default("")).Statements()
	status, err3 := NewDDL(engine).
		AddColumn(TableFiles, Column(FileStatus, t.Short).Default(StatusReady)).
		AddColumn(TableFiles, Column(FileStatusDetail, t.Text).Default("")).
		// A retry job or an admin page lists one status of a large scope
		// ("every failed file of this tenant").
		CreateIndex(TableFiles, Index{Name: "fs_files_scope_status", Columns: []IndexColumn{Key(FileScope), Key(FileStatus)}}).
		Statements()
	// Shared objects (blobs.go): every deletion asks whether another file
	// still names the object, by disk and key. MySQL caps an index key at
	// 3072 bytes, so it indexes the first objectKeyPrefix characters.
	object, err4 := NewDDL(engine).
		CreateIndex(TableFiles, Index{Name: "fs_files_object", Columns: []IndexColumn{Key(FileDisk), KeyPrefix(FileKey, objectKeyPrefix)}}).
		Statements()
	source, err5 := NewDDL(engine).
		AddColumn(TableFiles, Column(FileSourceChannel, t.Short).Default("")).
		AddColumn(TableFiles, Column(FileSourceClient, t.Text).Default("")).
		AddColumn(TableFiles, Column(FileSourceIP, t.Short).Default("")).
		Statements()
	// SweepExpired reads by expires_at across every scope.
	expiry, err6 := NewDDL(engine).
		AddColumn(TableFiles, Column(FileExpiresAt, t.Timestamp)).
		AddColumn(TableFiles, Column(FileViewOnce, t.Int).Default(0)).
		CreateIndex(TableFiles, Index{Name: "fs_files_expires", Columns: []IndexColumn{Key(FileExpiresAt)}}).
		Statements()
	// Extra digests (upload.Policy.Checksums): nullable, so files stored
	// before it, and files without any, read nil.
	checksums, err7 := NewDDL(engine).AddColumn(TableFiles, Column(FileChecksums, t.Doc)).Statements()
	return []Migration{
		{Owner: CoreOwner, Name: "0001_files_and_attachments", Statements: tables},
		{Owner: CoreOwner, Name: "0002_files_uploaded_by", Statements: uploadedBy},
		{Owner: CoreOwner, Name: "0003_files_status", Statements: status},
		{Owner: CoreOwner, Name: "0004_files_object", Statements: object},
		{Owner: CoreOwner, Name: "0005_files_source", Statements: source},
		{Owner: CoreOwner, Name: "0006_files_expiry", Statements: expiry},
		{Owner: CoreOwner, Name: "0007_files_checksums", Statements: checksums},
	}, errors.Join(err1, err2, err3, err4, err5, err6, err7)
}

// Migrations returns every migration in apply order: the core's, then each
// plugin's in installation order.
func (s *Store) Migrations() ([]Migration, error) {
	engine := engineOf(s.dialect)
	// The core is the first source, owned by CoreOwner; each Migrator
	// plugin follows, in installation order, through the same checks.
	type source struct {
		owner string
		list  func(engine string) ([]Migration, error)
	}
	sources := []source{{CoreOwner, coreMigrations}}
	for _, p := range s.plugins {
		if mg, ok := p.(Migrator); ok {
			sources = append(sources, source{p.Name(), mg.Migrations})
		}
	}
	var out []Migration
	for _, src := range sources {
		ms, err := src.list(engine)
		if err != nil {
			return nil, fmt.Errorf("records: %q migrations: %w", src.owner, err)
		}
		for _, m := range ms {
			if m.Owner != src.owner {
				return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: %q ships a migration owned by %q", src.owner, m.Owner)
			}
		}
		out = append(out, ms...)
	}
	return s.renamed(out)
}

// renamed applies WithTableNames to every statement of ms, after checking
// that the mapping gives every name ms and the ledger create a valid,
// distinct one. Without a mapping it returns ms as built.
func (s *Store) renamed(ms []Migration) ([]Migration, error) {
	if s.names == nil {
		return ms, nil
	}
	all, err := ledgerDDL(engineOf(s.dialect))
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		all = append(all, m.Statements...)
	}
	if err := s.checkNames(all); err != nil {
		return nil, err
	}
	out := make([]Migration, len(ms))
	for i, m := range ms {
		sts := make([]string, len(m.Statements))
		for j, st := range m.Statements {
			sts[j] = s.rename(st)
		}
		out[i] = Migration{Owner: m.Owner, Name: m.Name, Statements: sts}
	}
	return out, nil
}

// MigrationSQL renders every migration as one SQL script, for review or for
// applying with an external tool. The script creates the fs_migrations
// ledger first and records each migration in it after its statements,
// exactly as Migrate does, so a later Migrate sees the script's
// migrations as applied instead of running them again. The ledger inserts
// skip a migration already recorded, but a migration's own statements are
// not all repeatable (SQLite has no ADD COLUMN IF NOT EXISTS): apply the
// script once, to a database Migrate has not set up.
func (s *Store) MigrationSQL() (string, error) {
	engine := engineOf(s.dialect)
	ms, err := s.Migrations()
	ledger, lerr := s.ledgerDDL()
	if err = errors.Join(err, lerr); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("-- " + s.TableName(TableNameMigrations) + " (the ledger Migrate reads)\n")
	b.WriteString(strings.Join(ledger, ";\n"))
	b.WriteString(";\n\n")
	for _, m := range ms {
		fmt.Fprintf(&b, "-- %s %s\n", m.Owner, m.Name)
		for _, st := range m.Statements {
			b.WriteString(st)
			b.WriteString(";\n")
		}
		b.WriteString(s.rename(ledgerInsertSQL(engine, m)))
		b.WriteString(";\n\n")
	}
	return b.String(), nil
}

// ledgerInsertSQL is the statement recording m in the ledger with literal
// values, for MigrationSQL, skipping a migration already recorded. Owner
// and name come from the code, never from users, and are quoted anyway.
func ledgerInsertSQL(engine string, m Migration) string {
	quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }
	values := " INTO " + TableNameMigrations + " (owner, name, checksum, applied_at) VALUES (" +
		quote(m.Owner) + ", " + quote(m.Name) + ", " + quote(m.Checksum()) + ", CURRENT_TIMESTAMP)"
	if engine == EngineMySQL {
		return "INSERT IGNORE" + values
	}
	return "INSERT" + values + " ON CONFLICT (owner, name) DO NOTHING"
}

// migrationLockName is the name the Postgres advisory-lock key is derived from.
const migrationLockName = "ubgo_filesystem_records_migrate"

// mysqlLockName is the SQL expression naming the MySQL migration lock of
// the current database. MySQL lock names are server-wide (Postgres
// advisory locks are per database) and at most 64 characters, so the name
// carries a hash of the database name: two databases on one server, such
// as two apps, migrate independently instead of failing each other's
// bounded wait.
const mysqlLockName = "CONCAT('ubgo_fs_migrate_', LEFT(SHA1(COALESCE(DATABASE(), '')), 32))"

// migrationLockKey is the Postgres advisory-lock key derived from the name.
var migrationLockKey = func() int64 {
	sum := sha256.Sum256([]byte(migrationLockName))
	return int64(binary.BigEndian.Uint64(sum[:]))
}()

// Migrate applies every migration not yet in the ledger and returns the
// ones it applied, as "owner/name". It takes a cross-process lock
// (Postgres advisory lock, MySQL GET_LOCK; SQLite has one writer), so
// several instances starting together apply each migration once. A
// migration whose statements changed after it ran stops Migrate with
// CodeInvalidArgument.
func (s *Store) Migrate(ctx context.Context) ([]string, error) {
	ms, err := s.Migrations()
	if err != nil {
		return nil, err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	engine := engineOf(s.dialect)
	unlock, err := lockMigrations(ctx, conn, engine, s.lockWait)
	if err != nil {
		return nil, err
	}
	defer unlock()
	applied, err := s.readLedger(ctx, conn)
	if err != nil {
		return nil, err
	}
	var done []string
	for _, m := range ms {
		id := m.Owner + "/" + m.Name
		if sum, ok := applied[id]; ok {
			if sum != m.Checksum() {
				return done, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: migration %s changed after it was applied", id)
			}
			continue
		}
		if err := s.applyOne(ctx, conn, m); err != nil {
			return done, fmt.Errorf("records: migration %s: %w", id, err)
		}
		done = append(done, id)
	}
	return done, nil
}

// The ledger table, built like every other table (DDL). Its queries are
// written by hand in readLedger and applyOne, with these names.
var (
	tableMigrations   = sqlb.Table{Name: TableNameMigrations}
	migrationOwner    = sqlb.Col[string](tableMigrations, "owner")
	migrationName     = sqlb.Col[string](tableMigrations, "name")
	migrationChecksum = sqlb.Col[string](tableMigrations, "checksum")
	migrationApplied  = sqlb.Col[time.Time](tableMigrations, "applied_at")
)

// ledgerDDL creates the ledger table if it does not exist, with the
// Store's table names (WithTableNames).
func (s *Store) ledgerDDL() ([]string, error) {
	sts, err := ledgerDDL(engineOf(s.dialect))
	for i := range sts {
		sts[i] = s.rename(sts[i])
	}
	return sts, err
}

// ledgerDDL creates the ledger table, under its default name, if it does
// not exist.
func ledgerDDL(engine string) ([]string, error) {
	t := TypesFor(engine)
	return NewDDL(engine).CreateTable(Table{
		Table: tableMigrations,
		Columns: []ddl.ColumnSpec{
			Column(migrationOwner, t.Short), Column(migrationName, t.Key), Column(migrationChecksum, t.Short), Column(migrationApplied, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{migrationOwner, migrationName},
	}).Statements()
}

func (s *Store) readLedger(ctx context.Context, conn *sql.Conn) (map[string]string, error) {
	ledger, err := s.ledgerDDL()
	for _, st := range ledger {
		if err == nil {
			_, err = conn.ExecContext(ctx, st)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("records: create ledger: %w", err)
	}
	rows, err := conn.QueryContext(ctx, "SELECT owner, name, checksum FROM "+s.TableName(TableNameMigrations))
	if err != nil {
		return nil, fmt.Errorf("records: read ledger: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var owner, name, sum string
		if err := rows.Scan(&owner, &name, &sum); err != nil {
			return nil, err
		}
		out[owner+"/"+name] = sum
	}
	return out, rows.Err()
}

func lockMigrations(ctx context.Context, conn *sql.Conn, engine string, wait time.Duration) (func(), error) {
	switch engine {
	case EnginePostgres:
		if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
			return nil, fmt.Errorf("records: migration lock: %w", err)
		}
		return func() {
			_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockKey)
		}, nil
	case EngineMySQL:
		var got sql.NullInt64
		if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK("+mysqlLockName+", ?)", int64(wait/time.Second)).Scan(&got); err != nil {
			return nil, fmt.Errorf("records: migration lock: %w", err)
		}
		if !got.Valid || got.Int64 != 1 {
			return nil, filesystem.Errorf(filesystem.CodeUnavailable, "records: another process holds the migration lock")
		}
		return func() { _, _ = conn.ExecContext(context.Background(), "SELECT RELEASE_LOCK("+mysqlLockName+")") }, nil
	}
	return func() {}, nil
}

// applyOne runs a migration and records it, in one transaction where the
// engine allows; MySQL commits DDL implicitly, so there it runs statement
// by statement and records the migration last.
func (s *Store) applyOne(ctx context.Context, conn *sql.Conn, m Migration) error {
	engine, at, ledger := engineOf(s.dialect), s.clock(), s.TableName(TableNameMigrations)
	insert := "INSERT INTO " + ledger + " (owner, name, checksum, applied_at) VALUES (?, ?, ?, ?)"
	if engine == EnginePostgres {
		insert = "INSERT INTO " + ledger + " (owner, name, checksum, applied_at) VALUES ($1, $2, $3, $4)"
	}
	if engine == EngineMySQL {
		for _, st := range m.Statements {
			if _, err := conn.ExecContext(ctx, st); err != nil {
				return err
			}
		}
		_, err := conn.ExecContext(ctx, insert, m.Owner, m.Name, m.Checksum(), at)
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, st := range m.Statements {
		if _, err := tx.ExecContext(ctx, st); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, insert, m.Owner, m.Name, m.Checksum(), at); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
