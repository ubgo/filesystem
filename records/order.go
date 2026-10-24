package records

import (
	"encoding/base64"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/sqlb"
)

// FileOrder is the order Files lists a page in (FileQuery.Order). Every
// order is a keyset on (key, id), so pagination stays stable while files
// are added and fast at any depth.
type FileOrder string

// The closed set of file orders.
const (
	// OrderNewest is created_at then ID, descending: the default ("").
	OrderNewest FileOrder = ""
	// OrderOldest is created_at then ID, ascending.
	OrderOldest FileOrder = "oldest"
	// OrderName is filename then ID, ascending, in the database's
	// collation (case-insensitive on MySQL's default, byte order on
	// SQLite, the column's collation on Postgres).
	OrderName FileOrder = "name"
	// OrderNameDesc is filename then ID, descending.
	OrderNameDesc FileOrder = "name_desc"
	// OrderLargest is size then ID, descending.
	OrderLargest FileOrder = "largest"
	// OrderSmallest is size then ID, ascending.
	OrderSmallest FileOrder = "smallest"
)

// FileOrderValues lists every built-in FileOrder; WithFileOrder adds a
// Store's own.
var FileOrderValues = []FileOrder{OrderNewest, OrderOldest, OrderName, OrderNameDesc, OrderLargest, OrderSmallest}

// fileOrdering is how Files sorts and continues one FileOrder: the ORDER
// BY terms, the keyset predicate after a cursor's (value, id), and the
// cursor value of a file: value computes it from the File (built-in
// orders), or key reads it from the row the database returns (an
// Ordering's), so the cursor holds what the database compares.
type fileOrdering struct {
	terms []sqlb.OrderTerm
	after func(value, id string) (sqlb.Predicate, error)
	value func(File) string
	key   func() (sqlb.Binding, func() string)
}

// Ordering is a keyset order of a Store's own for Files (WithFileOrder):
// a case-insensitive name sort (sqlb.Lower(FileFilename)), a natural sort
// key, a collation, or any expression over the files table. Build it with
// KeysetOrder.
//
// Why it exists: the built-in orders sort a name in the database's
// collation, which is byte order on SQLite and case-sensitive on most
// Postgres collations; what a drive's users expect varies by product and
// language, so the expression is the application's.
type Ordering struct{ ord fileOrdering }

// KeysetOrder is an Ordering on (key, file ID) in dir: rows sort by key,
// ties by ID, and a page continues after the last row's (key, ID), so
// pagination stays stable at any depth. key is any expression over
// TableFiles' columns. The cursor holds the last row's key as the
// database computed it (selected beside the row), written with format and
// read back with parse, which must reverse it exactly: a cursor whose
// value parse refuses is CodeInvalidArgument. Index the expression (a
// functional index of the application's migration) when listings are
// large; without one the scope index narrows the rows and the database
// sorts them.
func KeysetOrder[T any](key sqlb.Expr[T], dir sqlb.SortDirection, parse func(string) (T, error), format func(T) string) Ordering {
	if parse == nil || format == nil {
		return Ordering{}
	}
	ord := keyset(key, dir, parse, nil)
	ord.key = func() (sqlb.Binding, func() string) {
		var v T
		return sqlb.Bind(key, &v), func() string { return format(v) }
	}
	return Ordering{ord: ord}
}

// customOrderPattern constrains WithFileOrder names: they appear in
// cursors, which split on cursorSeparator.
var customOrderPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// WithFileOrder adds a named order to the Store's Files: FileQuery.Order
// set to name lists in o (see KeysetOrder), with cursors that name it, so
// a cursor of another order is refused rather than skipping rows. name
// follows [a-z][a-z0-9_]*, at most 64 bytes, and is neither a built-in
// FileOrder nor added twice; o must come from KeysetOrder with a key,
// parse and format. Any of these wrong fails New with
// CodeInvalidArgument. The built-in orders are unchanged.
//
// Example, names case-insensitively in the database's own lowering:
//
//	records.WithFileOrder("name_ci", records.KeysetOrder(sqlb.Lower(records.FileFilename),
//		sqlb.SortAsc, func(v string) (string, error) { return v, nil }, func(v string) string { return v }))
func WithFileOrder(name FileOrder, o Ordering) Option {
	return func(s *Store) error {
		if !customOrderPattern.MatchString(string(name)) || slices.Contains(FileOrderValues, name) || s.orders[name].terms != nil {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: file order %q must match %s, not be built in (%q) and be added once", name, customOrderPattern, FileOrderValues)
		}
		if o.ord.key == nil {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: file order %q needs an Ordering from KeysetOrder with a key, parse and format", name)
		}
		if s.orders == nil {
			s.orders = map[FileOrder]fileOrdering{}
		}
		s.orders[name] = o.ord
		return nil
	}
}

// keyset builds a fileOrdering on (key, FileID) in dir, parsing a cursor
// value with parse and writing one with format.
func keyset[T any](key sqlb.Expr[T], dir sqlb.SortDirection, parse func(string) (T, error), format func(File) string) fileOrdering {
	return fileOrdering{
		terms: []sqlb.OrderTerm{sqlb.OrderBy(key, dir), sqlb.OrderBy(FileID, dir)},
		after: func(value, id string) (sqlb.Predicate, error) {
			v, err := parse(value)
			if err != nil {
				return nil, errInvalidCursor()
			}
			row, at := sqlb.Row(key, FileID), sqlb.Row(sqlb.Param(v), sqlb.Param(id))
			if dir == sqlb.SortDesc {
				return sqlb.RowLess(row, at), nil
			}
			return sqlb.RowGreater(row, at), nil
		},
		value: format,
	}
}

func parseMicros(v string) (time.Time, error) {
	n, err := strconv.ParseInt(v, 10, 64)
	return time.UnixMicro(n).UTC(), err
}

func parseString(v string) (string, error) { return v, nil }

func parseSize(v string) (int64, error) { return strconv.ParseInt(v, 10, 64) }

func microsOf(f File) string { return strconv.FormatInt(f.CreatedAt.UnixMicro(), 10) }
func nameOf(f File) string   { return f.Filename }
func sizeOf(f File) string   { return strconv.FormatInt(f.Size, 10) }

// orderingOf returns the ordering of o: a built-in one, or one of
// WithFileOrder; CodeInvalidArgument for any other.
func (s *Store) orderingOf(o FileOrder) (fileOrdering, error) {
	if ord, ok := s.orders[o]; ok {
		return ord, nil
	}
	switch o {
	case OrderNewest:
		return keyset(FileCreatedAt, sqlb.SortDesc, parseMicros, microsOf), nil
	case OrderOldest:
		return keyset(FileCreatedAt, sqlb.SortAsc, parseMicros, microsOf), nil
	case OrderName:
		return keyset(FileFilename, sqlb.SortAsc, parseString, nameOf), nil
	case OrderNameDesc:
		return keyset(FileFilename, sqlb.SortDesc, parseString, nameOf), nil
	case OrderLargest:
		return keyset(FileSize, sqlb.SortDesc, parseSize, sizeOf), nil
	case OrderSmallest:
		return keyset(FileSize, sqlb.SortAsc, parseSize, sizeOf), nil
	}
	return fileOrdering{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: unknown file order %q (one of %q, or a Store's WithFileOrder)", o, FileOrderValues)
}

func errInvalidCursor() error {
	return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: invalid cursor")
}

// encodeOrderCursor is the cursor after f in order o. OrderNewest keeps
// EncodeCursor's format, so cursors issued before orders existed still
// work; every other order names itself in the cursor, so a cursor is
// refused by a query in another order instead of skipping rows.
//
// key is the row's key as the database returned it, for an Ordering
// (fileOrdering.key); built-in orders compute it from f instead.
func encodeOrderCursor(o FileOrder, ord fileOrdering, f File, key string) string {
	if o == OrderNewest {
		return EncodeCursor(f.CreatedAt, f.ID)
	}
	if ord.key == nil {
		key = ord.value(f)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(string(o) + cursorSeparator + key + cursorSeparator + f.ID))
}

// decodeOrderCursor reverses encodeOrderCursor into the keyset predicate.
// The value may hold the separator (a filename can); the order before it
// and the ID after it cannot, so the outer separators split it.
func decodeOrderCursor(o FileOrder, ord fileOrdering, c string) (sqlb.Predicate, error) {
	if o == OrderNewest {
		at, id, err := DecodeCursor(c)
		if err != nil {
			return nil, err
		}
		return sqlb.RowLess(sqlb.Row(FileCreatedAt, FileID), sqlb.Row(sqlb.Param(at), sqlb.Param(id))), nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return nil, errInvalidCursor()
	}
	name, rest, ok := strings.Cut(string(raw), cursorSeparator)
	cut := strings.LastIndex(rest, cursorSeparator)
	if !ok || name != string(o) || cut < 0 || cut == len(rest)-1 {
		return nil, errInvalidCursor()
	}
	return ord.after(rest[:cut], rest[cut+1:])
}
