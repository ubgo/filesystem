package records

import (
	"errors"
	"strings"
	"testing"

	"github.com/ubgo/sqlb"
)

// unbuildable is a Statement whose Build fails.
type unbuildable struct{}

func (unbuildable) Build() (sqlb.Query, error) { return sqlb.Query{}, errors.New("no") }

// TestOneLockedBuildError: a statement that cannot be built is returned
// by oneLocked and eachLocked, as One and Each return it, before any SQL
// runs.
func TestOneLockedBuildError(t *testing.T) {
	s := &Store{dialect: sqlb.SQLiteDialect{}}
	if err := s.oneLocked(t.Context(), unbuildable{}, lockShare); err == nil || !strings.Contains(err.Error(), "records: build") {
		t.Fatalf("oneLocked = %v", err)
	}
	if err := s.eachLocked(t.Context(), unbuildable{}, lockUpdate, func() error { t.Fatal("a row of an unbuilt read"); return nil }); err == nil || !strings.Contains(err.Error(), "records: build") {
		t.Fatalf("eachLocked = %v", err)
	}
}

// TestLockedQuery: every locking read (oneLocked, lockObjectRows) gets
// its mode's clause on engines with row locks and none on SQLite.
func TestLockedQuery(t *testing.T) {
	for _, d := range []sqlb.Dialect{sqlb.PostgresDialect{}, sqlb.MySQLDialect{}, sqlb.SQLiteDialect{}} {
		s := &Store{dialect: d}
		var id string
		q, err := s.lockedQuery(sqlb.NewSelect(d).Columns(sqlb.Bind(FileID, &id)).From(TableFiles), lockShare)
		if err != nil {
			t.Fatal(err)
		}
		locked := strings.HasSuffix(q.SQL, " "+string(lockShare))
		if locked != (engineOf(d) != EngineSQLite) {
			t.Errorf("%s: %s", sqlb.DialectName(d), q.SQL)
		}
	}
}
