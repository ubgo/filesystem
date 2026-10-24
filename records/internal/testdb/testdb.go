// Package testdb is the records module's own name for recordstest, the
// exported engine-aware test helper: it keeps the module's tests' imports
// short and stable while the implementation lives in one place.
package testdb

import (
	"database/sql"
	"testing"

	"github.com/ubgo/filesystem/records/recordstest"
)

// Environment of the engine tests; see recordstest.
const (
	EnvEngine      = recordstest.EnvEngine
	EnvPostgresDSN = recordstest.EnvPostgresDSN
	EnvMySQLDSN    = recordstest.EnvMySQLDSN
)

// Engine names, as sqlb reports them.
const (
	Postgres = recordstest.Postgres
	MySQL    = recordstest.MySQL
	SQLite   = recordstest.SQLite
)

// ErrInjected is the error of a step Faults made fail.
var ErrInjected = recordstest.ErrInjected

// Faults counts database steps and can make one fail; see recordstest.Faults.
type Faults = recordstest.Faults

// Engine, Dialect, OpenFaulty, Exec and OnlySQLite are recordstest's.
var (
	Engine     = recordstest.Engine
	Dialect    = recordstest.Dialect
	OpenFaulty = recordstest.OpenFaulty
	Exec       = recordstest.Exec
	OnlySQLite = recordstest.OnlySQLite
)

// Open returns a fresh, empty database for one test (recordstest.Open
// without the dialect, which the records tests take from Dialect).
func Open(t testing.TB) *sql.DB {
	t.Helper()
	db, _ := recordstest.Open(t)
	return db
}
