package sqlblob

import (
	"regexp"

	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
	"github.com/ubgo/sqlb/mysql"
	"github.com/ubgo/sqlb/pg"
	"github.com/ubgo/sqlb/sqlite"

	"github.com/ubgo/filesystem"
)

// DefaultTable is the table name New uses when Config.Table is empty.
const DefaultTable = "filesystem_blobs"

// tableName is what Config.Table and Config.Schema may be: a plain
// identifier, so a name taken from configuration can never smuggle SQL
// into the DDL.
var tableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

// The table's column names.
const (
	colKey                = "key"
	colData               = "data"
	colSize               = "size"
	colSHA256             = "sha256"
	colContentType        = "content_type"
	colVisibility         = "visibility"
	colMetadata           = "metadata"
	colCacheControl       = "cache_control"
	colContentDisposition = "content_disposition"
	colContentEncoding    = "content_encoding"
	colModified           = "modified_ns"
)

// MySQL table options: byte-exact comparison everywhere, as on the other
// engines (the key column is VARBINARY for the same reason: a _bin
// collation still ignores trailing spaces).
const (
	mysqlCharset   = "utf8mb4"
	mysqlCollation = "utf8mb4_bin"
)

// columns are one table's typed columns. They are built per Driver from
// Config.Schema and Config.Table, so two drivers over two tables share
// nothing.
type columns struct {
	table              sqlb.Table
	key                sqlb.Expr[string]
	data               sqlb.Expr[[]byte]
	size               sqlb.Expr[int64]
	sha256             sqlb.Expr[string]
	contentType        sqlb.Expr[string]
	visibility         sqlb.Expr[string]
	metadata           sqlb.Expr[string]
	cacheControl       sqlb.Expr[string]
	contentDisposition sqlb.Expr[string]
	contentEncoding    sqlb.Expr[string]
	modified           sqlb.Expr[int64]
}

func newColumns(schema, name string) columns {
	t := sqlb.Table{Name: name, Schema: schema}
	return columns{
		table:              t,
		key:                sqlb.Col[string](t, colKey),
		data:               sqlb.Col[[]byte](t, colData),
		size:               sqlb.Col[int64](t, colSize),
		sha256:             sqlb.Col[string](t, colSHA256),
		contentType:        sqlb.Col[string](t, colContentType),
		visibility:         sqlb.Col[string](t, colVisibility),
		metadata:           sqlb.Col[string](t, colMetadata),
		cacheControl:       sqlb.Col[string](t, colCacheControl),
		contentDisposition: sqlb.Col[string](t, colContentDisposition),
		contentEncoding:    sqlb.Col[string](t, colContentEncoding),
		modified:           sqlb.Col[int64](t, colModified),
	}
}

// sqlTypes are one engine's spellings of the table's column types.
type sqlTypes struct{ key, blob, integer, text string }

// typesFor returns the column types of engine. The key compares byte by
// byte on every engine (Postgres's "C" collation, MySQL's VARBINARY,
// SQLite's default BINARY), so ORDER BY key is the order List promises and
// a range over a directory's prefix holds exactly the keys inside it.
func typesFor(engine string) sqlTypes {
	switch engine {
	case sqlb.DialectPostgres:
		return sqlTypes{key: `TEXT COLLATE "C"`, blob: "BYTEA", integer: "BIGINT", text: "TEXT"}
	case sqlb.DialectMySQL:
		return sqlTypes{key: "VARBINARY(1024)", blob: "LONGBLOB", integer: "BIGINT", text: "TEXT"}
	}
	return sqlTypes{key: "TEXT", blob: "BLOB", integer: "INTEGER", text: "TEXT"}
}

func column[T any](col sqlb.Expr[T], sqlType string) ddl.ColumnDef[T] {
	return ddl.Column(col).As(ddl.RawType[T](sqlType))
}

// Schema returns the CREATE TABLE IF NOT EXISTS statement for the driver's
// table on its engine, for an application that runs its own migrations.
// Migrate runs the same statement. The key column is the primary key and
// holds MaxKeyLength (1024) bytes.
func (d *Driver) Schema() ([]string, error) {
	c, ty := d.cols, typesFor(d.engine)
	defs := []ddl.ColumnSpec{
		column(c.key, ty.key),
		column(c.data, ty.blob),
		column(c.size, ty.integer),
		column(c.sha256, ty.text),
		column(c.contentType, ty.text),
		column(c.visibility, ty.text),
		column(c.metadata, ty.text),
		column(c.cacheControl, ty.text),
		column(c.contentDisposition, ty.text),
		column(c.contentEncoding, ty.text),
		column(c.modified, ty.integer),
	}
	var script ddl.Script
	var err error
	switch d.engine {
	case sqlb.DialectPostgres:
		script, err = pg.CreateTable(c.table).IfNotExists().Column(defs[0], defs[1:]...).Clause(pg.PrimaryKey(c.key)).Build()
	case sqlb.DialectMySQL:
		script, err = mysql.CreateTable(c.table).IfNotExists().Column(defs[0], defs[1:]...).
			Clause(mysql.PrimaryKey(c.key), mysql.InnoDB(), mysql.DefaultCharset(mysqlCharset), mysql.TableCollate(mysqlCollation)).Build()
	default:
		script, err = sqlite.CreateTable(c.table).IfNotExists().Column(defs[0], defs[1:]...).Clause(sqlite.PrimaryKey(c.key)).Build()
	}
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	return script.SQL(), nil
}
