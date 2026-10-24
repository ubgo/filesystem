package locks

import (
	"errors"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/sqlb"
)

// TestForUpdate: the guards' reads lock rows on every engine that has row
// locks, and leave SQLite's SQL alone.
func TestForUpdate(t *testing.T) {
	mgr, _ := filesystem.NewManager()
	for _, d := range []sqlb.Dialect{sqlb.PostgresDialect{}, sqlb.MySQLDialect{}, sqlb.SQLiteDialect{}} {
		s, err := records.New(testdb.Open(t), d, mgr) // never queried: forUpdate renders only
		if err != nil {
			t.Fatal(err)
		}
		var l Lock
		st := sqlb.NewSelect(d).Columns(bindings(&l)...).From(Table).Where(sqlb.Eq(Scope, "t"))
		q, err := forUpdate(s, st)
		if err != nil {
			t.Fatal(err)
		}
		locked := strings.HasSuffix(q.SQL, " FOR UPDATE")
		if locked != (sqlb.DialectName(d) != records.EngineSQLite) {
			t.Errorf("%s: %s", sqlb.DialectName(d), q.SQL)
		}
	}
}

// unbuildable is a Statement whose Build fails.
type unbuildable struct{}

var errUnbuildable = errors.New("unbuildable")

func (unbuildable) Build() (sqlb.Query, error) { return sqlb.Query{}, errUnbuildable }

// TestLockedOneBuildError: a locking read that cannot be built is
// returned before any SQL runs, never run without its FOR UPDATE.
func TestLockedOneBuildError(t *testing.T) {
	mgr, _ := filesystem.NewManager()
	s, err := records.New(testdb.Open(t), testdb.Dialect(), mgr)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockedOne(t.Context(), s, unbuildable{}); !errors.Is(err, errUnbuildable) {
		t.Fatalf("lockedOne = %v", err)
	}
}
