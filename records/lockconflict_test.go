package records_test

import (
	"errors"
	"testing"

	"github.com/ubgo/filesystem/records"
)

type sqlState string

func (s sqlState) Error() string    { return "pg error " + string(s) }
func (s sqlState) SQLState() string { return string(s) }

// TestIsLockConflict recognises a database aborting a transaction over a
// lock conflict on every engine, and nothing else.
func TestIsLockConflict(t *testing.T) {
	for _, err := range []error{
		sqlState("40P01"), sqlState("40001"),
		errors.New("Error 1213 (40001): Deadlock found when trying to get lock; try restarting transaction"),
		errors.New("Error 1205 (HY000): Lock wait timeout exceeded"),
		errors.New("database is locked (5) (SQLITE_BUSY)"),
	} {
		if !records.IsLockConflict(err) {
			t.Errorf("%v is a lock conflict", err)
		}
	}
	for _, err := range []error{nil, sqlState("23505"), errors.New("Error 1062: Duplicate entry")} {
		if records.IsLockConflict(err) {
			t.Errorf("%v is not a lock conflict", err)
		}
	}
}
