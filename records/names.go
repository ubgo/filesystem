package records

import (
	"regexp"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/sqlb"
)

// ObjectPrefix starts the name of every table and index this module and
// its plugins create (fs_files, fs_acl_grants, fs_files_scope_created),
// and no other word of the SQL they write: no column, alias or keyword
// starts with it. WithTableNames relies on that invariant, so a plugin's
// own tables follow it too (a table named otherwise keeps its name under
// any mapping).
const ObjectPrefix = "fs_"

// MaxObjectNameBytes is the longest table or index name a mapping may
// produce: Postgres keeps 63 bytes of an identifier and cuts the rest
// silently, which would make two long names one.
const MaxObjectNameBytes = 63

// MaxTablePrefixBytes is the longest WithTablePrefix: the longest name
// the module ships (fs_webhook_deliveries_endpoint, 30 bytes) plus the
// prefix stays within MaxObjectNameBytes.
const MaxTablePrefixBytes = 32

// TableNamer maps the name of a table or index of this module or a plugin
// (always starting with ObjectPrefix: "fs_files", "fs_acl_grants") to the
// name it has in the application's database. It must be pure and
// injective (two names never map to one), and return an identifier of
// letters, digits and "_" not starting with a digit, of at most
// MaxObjectNameBytes; Migrate checks every name it creates against these
// rules (CodeInvalidArgument).
type TableNamer func(name string) string

// objectNamePattern finds the names a TableNamer maps in rendered SQL:
// whole words starting with ObjectPrefix, quoted or not. The rendered SQL
// of a query carries values as placeholders, never inline, so the only
// such words are object names.
var objectNamePattern = regexp.MustCompile(`\b` + ObjectPrefix + `[A-Za-z0-9_]*`)

// identifierPattern is what a mapped name must be.
var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// tablePrefixPattern is what WithTablePrefix accepts.
var tablePrefixPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// ddlStatementName names the prepared statement and session variable the
// MySQL migration guards use (see guarded): it starts with ObjectPrefix
// but is not an object, so no mapping renames it.
const ddlStatementName = ObjectPrefix + "ddl"

// WithTableNames gives every table and index of the module and its
// plugins the name fn maps it to: two Stores in one database (one per
// product, or a test beside a live one), a house naming rule
// ("storage_files"), or a database whose fs_ names are taken. Queries,
// migrations and the migration ledger all use the mapped names; without
// it the names are the defaults, unchanged.
//
// Why a mapping applied to rendered SQL rather than to the typed table
// values: the typed columns (FileID, acl.Subject) are package values that
// carry their table, shared by every Store in the process, so a per-Store
// name can only be applied where a Store renders a statement. Every
// statement the module runs passes Store.Build, Exec, One or Each, and
// every migration passes Migrations, so the mapping reaches them all,
// plugins' included. SQL an application writes itself against these
// tables names them with Store.TableName.
//
// Pick the names before the first Migrate and keep them: the tables are
// not renamed for an existing database. A nil fn is CodeInvalidArgument.
func WithTableNames(fn TableNamer) Option {
	return func(s *Store) error {
		if fn == nil {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: nil table namer")
		}
		s.names = fn
		return nil
	}
}

// WithTablePrefix is WithTableNames putting prefix before every name:
// WithTablePrefix("app_") makes fs_files app_fs_files and every index
// app_fs_…. prefix is 1 to MaxTablePrefixBytes of [a-z0-9_], not starting
// with a digit (CodeInvalidArgument otherwise).
func WithTablePrefix(prefix string) Option {
	return func(s *Store) error {
		if len(prefix) > MaxTablePrefixBytes || !tablePrefixPattern.MatchString(prefix) {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: table prefix %q must match %s, at most %d bytes", prefix, tablePrefixPattern, MaxTablePrefixBytes)
		}
		return WithTableNames(func(name string) string { return prefix + name })(s)
	}
}

// TableName is the name the table or index name (a default name, such as
// TableNameFiles or acl.TableNameGrants) has in this Store's database:
// name itself unless WithTableNames maps it. Use it in SQL the
// application writes against the fs_ tables.
func (s *Store) TableName(name string) string {
	if s.names == nil || name == ddlStatementName || !strings.HasPrefix(name, ObjectPrefix) {
		return name
	}
	return s.names(name)
}

// rename applies WithTableNames to rendered SQL: every object name in it
// becomes the mapped name. Without a mapping it returns sql unchanged.
func (s *Store) rename(sql string) string {
	if s.names == nil {
		return sql
	}
	return objectNamePattern.ReplaceAllStringFunc(sql, s.TableName)
}

// checkNames verifies the mapping of every object name in statements:
// each maps to a valid identifier of at most MaxObjectNameBytes, and no
// two map to one. Migrations calls it on everything it renders, so a bad
// TableNamer fails before any table is created.
func (s *Store) checkNames(statements []string) error {
	if s.names == nil {
		return nil
	}
	seen := map[string]string{}
	for _, st := range statements {
		for _, name := range objectNamePattern.FindAllString(st, -1) {
			to := s.TableName(name)
			if len(to) > MaxObjectNameBytes || !identifierPattern.MatchString(to) {
				return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: table name %q maps to %q, not an identifier of at most %d bytes", name, to, MaxObjectNameBytes)
			}
			if from, ok := seen[to]; ok && from != name {
				return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: table names %q and %q both map to %q", from, name, to)
			}
			seen[to] = name
		}
	}
	return nil
}

// Build renders st with the Store's table names (WithTableNames): what
// Exec, One and Each run. A plugin that runs a statement any other way
// (a locking read with its own clause) builds it here, so its SQL names
// the same tables.
func (s *Store) Build(st Statement) (sqlb.Query, error) {
	q, err := st.Build()
	if err != nil {
		return sqlb.Query{}, err
	}
	q.SQL = s.rename(q.SQL)
	return q, nil
}
