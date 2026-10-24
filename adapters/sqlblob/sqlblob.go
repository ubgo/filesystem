// Package sqlblob is a filesystem.Driver that keeps objects as rows of one
// SQL table: key, bytes, size, SHA-256, content type, visibility, metadata,
// HTTP headers and modification time. It runs on SQLite, Postgres and MySQL
// through github.com/ubgo/sqlb and the application's own *sql.DB, so a small
// deployment can store avatars, exports or attachments in the database it
// already backs up, with no object store to run.
//
// It is a separate module that requires only the library and sqlb; it
// imports no SQL driver, the application brings its own. The table is
// created by Migrate (or the statement Schema returns, for an application's
// own migration tool); New never runs DDL.
//
// A write reads the whole body into memory and stores it in one statement,
// so readers see the old object or the new one, never a mix. A body larger
// than Config.MaxObjectSize is refused with CodeTooLarge before anything is
// stored. Ranged reads select only the requested bytes (substr), so a range
// costs memory for the range, not the object.
package sqlblob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/ubgo/sqlb"

	"github.com/ubgo/filesystem"
)

// DefaultMaxObjectSize is Config.MaxObjectSize when it is zero: 16 MiB.
// Every write holds its body in memory and a database is a poor home for
// large media, so the cap is low; raise it deliberately. On MySQL it must
// stay below the server's max_allowed_packet.
const DefaultMaxObjectSize int64 = 16 << 20

// Config configures a Driver.
type Config struct {
	// DB is the database holding the table. Required. The driver never
	// closes it: the application owns the pool.
	DB *sql.DB
	// Dialect is the database's sqlb dialect (sqlb.SQLiteDialect{},
	// sqlb.PostgresDialect{} or sqlb.MySQLDialect{}). Required.
	Dialect sqlb.Dialect
	// Table is the table's name, a plain identifier; "" means
	// DefaultTable. Two drivers over two tables are two independent disks.
	Table string
	// Schema qualifies Table with a schema, a plain identifier like
	// Table; "" leaves the name unqualified (the connection's search path
	// or current database decides). On Postgres it is the schema
	// ("storage"."filesystem_blobs"), on MySQL the database, on SQLite an
	// attached database's name. Schema and Migrate create the table there
	// but not the schema itself, which the application creates first.
	Schema string
	// Clock is the time source for each row's modification time (what
	// Stat and List report as ModTime); nil means time.Now. Pass a fixed
	// or stepping clock for deterministic tests, or a
	// database-synchronised one when several application servers with
	// skewed clocks write the same table.
	Clock func() time.Time
	// MaxObjectSize caps one object's bytes; 0 means DefaultMaxObjectSize.
	MaxObjectSize int64
}

// Driver is the SQL table backend. Create it with New; it is safe for
// concurrent use as far as the *sql.DB is.
type Driver struct {
	db      *sql.DB
	dialect sqlb.Dialect
	engine  string
	cols    columns
	maxSize int64
	// clock is Config.Clock, or time.Now.
	clock func() time.Time
}

// New checks cfg and returns a driver over its table. It runs no SQL: call
// Migrate (or run Schema's statement) once before the first use.
func New(cfg Config) (*Driver, error) {
	if cfg.DB == nil || cfg.Dialect == nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "sqlblob: DB and Dialect are required")
	}
	engine := sqlb.DialectName(cfg.Dialect)
	switch engine {
	case sqlb.DialectSQLite, sqlb.DialectPostgres, sqlb.DialectMySQL:
	default:
		return nil, filesystem.Errorf(filesystem.CodeUnsupported, "sqlblob: dialect %q is not supported (sqlite, postgres, mysql)", engine)
	}
	table := cfg.Table
	if table == "" {
		table = DefaultTable
	}
	if !tableName.MatchString(table) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "sqlblob: table name %q is not a plain identifier", table)
	}
	if cfg.Schema != "" && !tableName.MatchString(cfg.Schema) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "sqlblob: schema name %q is not a plain identifier", cfg.Schema)
	}
	if cfg.MaxObjectSize < 0 {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "sqlblob: MaxObjectSize is negative")
	}
	maxSize := cfg.MaxObjectSize
	if maxSize == 0 {
		maxSize = DefaultMaxObjectSize
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Driver{db: cfg.DB, dialect: cfg.Dialect, engine: engine, cols: newColumns(cfg.Schema, table), maxSize: maxSize, clock: clock}, nil
}

// Migrate creates the table if it does not exist. It is safe to run on
// every start.
func (d *Driver) Migrate(ctx context.Context) error {
	stmts, err := d.Schema()
	if err != nil {
		return err
	}
	for _, s := range stmts {
		if _, err := d.db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// Native returns the *sql.DB, for the filesystem.As escape hatch (a query
// the driver does not offer, such as the total bytes stored).
func (d *Driver) Native() *sql.DB { return d.db }

// Capabilities reports what a table can do: every operation is one or two
// statements, so copy, move, conditional writes and compare-and-swap are
// all native and atomic.
func (d *Driver) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{
		Copy:             true,
		Move:             true,
		SetVisibility:    true,
		DeletePrefix:     true,
		DeleteKeys:       true,
		Metadata:         true,
		ContentType:      true,
		Headers:          true,
		ConditionalWrite: true,
		CompareAndSwap:   true,
	}
}

// object is one row without its bytes, as Stat and List read it.
type object struct {
	key, sha, contentType, visibility, metadata string
	cacheControl, contentDisposition            string
	contentEncoding                             string
	size, modified                              int64
}

// bindings binds every column but data to o, in one fixed order.
func (d *Driver) bindings(o *object) []sqlb.Binding {
	c := d.cols
	return []sqlb.Binding{
		sqlb.Bind(c.key, &o.key), sqlb.Bind(c.size, &o.size), sqlb.Bind(c.sha256, &o.sha),
		sqlb.Bind(c.contentType, &o.contentType), sqlb.Bind(c.visibility, &o.visibility),
		sqlb.Bind(c.metadata, &o.metadata), sqlb.Bind(c.cacheControl, &o.cacheControl),
		sqlb.Bind(c.contentDisposition, &o.contentDisposition), sqlb.Bind(c.contentEncoding, &o.contentEncoding),
		sqlb.Bind(c.modified, &o.modified),
	}
}

// entry turns a row into an Entry. A metadata column that does not decode
// is reported, not dropped, so a row edited by hand is never served with
// half its attributes.
func (o *object) entry() (filesystem.Entry, error) {
	var md map[string]string
	if o.metadata != "" {
		if err := json.Unmarshal([]byte(o.metadata), &md); err != nil {
			return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeUnknown, "sqlblob: metadata of %q does not decode: %v", o.key, err)
		}
	}
	return filesystem.Entry{
		Key:         o.key,
		Size:        o.size,
		ModTime:     time.Unix(0, o.modified).UTC(),
		ContentType: o.contentType,
		ETag:        o.sha,
		SHA256:      o.sha,
		Visibility:  filesystem.Visibility(o.visibility),
		Metadata:    md,
		Headers: filesystem.HTTPHeaders{
			CacheControl:       o.cacheControl,
			ContentDisposition: o.contentDisposition,
			ContentEncoding:    o.contentEncoding,
		},
	}, nil
}

func notFound(key string) error {
	return filesystem.Errorf(filesystem.CodeNotFound, "sqlblob: no object %q", key)
}

// stat reads key's row through db (the pool, or a transaction).
func (d *Driver) stat(ctx context.Context, db sqlb.DBTX, key string) (filesystem.Entry, error) {
	var o object
	if err := one(ctx, db, sqlb.NewSelect(d.dialect).Columns(d.bindings(&o)...).From(d.cols.table).Where(sqlb.Eq(d.cols.key, key))); err != nil {
		if errors.Is(err, sqlb.ErrNoRows) {
			return filesystem.Entry{}, notFound(key)
		}
		return filesystem.Entry{}, err
	}
	return o.entry()
}

// Stat implements filesystem.Driver.
func (d *Driver) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	return d.stat(ctx, d.db, key)
}

// Read implements filesystem.Driver. Only the range's bytes leave the
// database: substr is 1-based, and without a length it reads to the end. A
// range past the end reads nothing.
func (d *Driver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	args := []sqlb.SQLer{d.cols.data, sqlb.Param(rng.Offset + 1)}
	if rng.Length > 0 {
		args = append(args, sqlb.Param(rng.Length))
	}
	var data []byte
	q := sqlb.NewSelect(d.dialect).Columns(sqlb.Bind(sqlb.Fn[[]byte]("substr", args...), &data)).
		From(d.cols.table).Where(sqlb.Eq(d.cols.key, key))
	if err := one(ctx, d.db, q); err != nil {
		if errors.Is(err, sqlb.ErrNoRows) {
			return nil, notFound(key)
		}
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// Write implements filesystem.Driver. The body is read in full (at most
// MaxObjectSize bytes) before any statement runs, so a failing body stores
// nothing; the row is then written in one statement.
func (d *Driver) Write(ctx context.Context, key string, body io.Reader, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	data, err := d.readBody(ctx, body)
	if err != nil {
		return filesystem.Entry{}, err
	}
	md := ""
	if len(opts.Metadata) > 0 {
		raw, err := json.Marshal(opts.Metadata)
		if err != nil {
			return filesystem.Entry{}, filesystem.NewError(filesystem.CodeInvalidArgument, err)
		}
		md = string(raw)
	}
	sum := sha256.Sum256(data)
	r := row{
		object: object{
			key: key, size: int64(len(data)), sha: hex.EncodeToString(sum[:]), contentType: opts.ContentType,
			visibility: string(opts.Visibility), metadata: md, cacheControl: opts.Headers.CacheControl,
			contentDisposition: opts.Headers.ContentDisposition, contentEncoding: opts.Headers.ContentEncoding,
			modified: d.nowNanos(),
		},
		data: data,
	}
	switch {
	case opts.IfNotExists:
		err = d.insert(ctx, r)
	case opts.IfMatch != "":
		err = d.swap(ctx, r, opts.IfMatch)
	default:
		err = d.upsert(ctx, d.db, r)
	}
	if err != nil {
		return filesystem.Entry{}, err
	}
	return r.entry()
}

// readBody reads body in full, refusing one over the cap and stopping when
// ctx ends. The slice is never nil, so an empty object is an empty value,
// not NULL.
func (d *Driver) readBody(ctx context.Context, body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(&ctxReader{ctx: ctx, r: body}, d.maxSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > d.maxSize {
		return nil, filesystem.Errorf(filesystem.CodeTooLarge, "sqlblob: object exceeds MaxObjectSize (%d bytes)", d.maxSize)
	}
	return data, nil
}

// row is a full row: the attributes and the bytes.
type row struct {
	object
	data []byte
}

// assignments sets every column of r.
func (d *Driver) assignments(r row) []sqlb.Assignment {
	c := d.cols
	return []sqlb.Assignment{
		sqlb.Set(c.key, r.key), sqlb.Set(c.data, r.data), sqlb.Set(c.size, r.size), sqlb.Set(c.sha256, r.sha),
		sqlb.Set(c.contentType, r.contentType), sqlb.Set(c.visibility, r.visibility), sqlb.Set(c.metadata, r.metadata),
		sqlb.Set(c.cacheControl, r.cacheControl), sqlb.Set(c.contentDisposition, r.contentDisposition),
		sqlb.Set(c.contentEncoding, r.contentEncoding), sqlb.Set(c.modified, r.modified),
	}
}

// insert writes r only if its key is free. A failed insert whose key now
// exists lost the race (or found the key taken): CodeExists. Asking again
// rather than parsing the error keeps the driver free of SQL driver
// imports, whose duplicate-key errors differ per engine.
func (d *Driver) insert(ctx context.Context, r row) error {
	as := d.assignments(r)
	if _, err := exec(ctx, d.db, sqlb.NewInsert(d.dialect).Into(d.cols.table).Row(as...)); err != nil {
		if _, serr := d.stat(ctx, d.db, r.key); serr == nil {
			return filesystem.NewError(filesystem.CodeExists, err)
		}
		return err
	}
	return nil
}

// swap replaces key's row only while its SHA-256 is still etag. No row
// changed means the object changed or is gone (CodeConflict), except on
// MySQL, which counts a row whose values did not change as unaffected: a
// row still carrying etag is then a success.
func (d *Driver) swap(ctx context.Context, r row, etag string) error {
	as := d.assignments(r)
	q := sqlb.NewUpdate(d.dialect).Table(d.cols.table).Set(as...).
		Where(sqlb.Eq(d.cols.key, r.key), sqlb.Eq(d.cols.sha256, etag))
	n, err := exec(ctx, d.db, q)
	if err != nil || n > 0 {
		return err
	}
	if e, serr := d.stat(ctx, d.db, r.key); serr == nil && e.ETag == etag && e.ETag == r.sha {
		return nil
	}
	return filesystem.Errorf(filesystem.CodeConflict, "sqlblob: object %q changed since ETag %s", r.key, etag)
}

// upsert writes r whether or not its key exists, in one statement: each
// engine's own upsert, the only part of the driver spelled per engine.
func (d *Driver) upsert(ctx context.Context, db sqlb.DBTX, r row) error {
	_, err := exec(ctx, db, d.upsertStatement(r))
	return err
}

// ctxReader fails with the context's error once the context ends, so a long
// body stops being read when the caller gives up.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// nowNanos is a row's modification time from Config.Clock, stored as Unix
// nanoseconds so it round-trips identically on every engine (no time zone,
// no precision loss).
func (d *Driver) nowNanos() int64 { return d.clock().UnixNano() }
