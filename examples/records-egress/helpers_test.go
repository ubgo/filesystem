package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/egress"
)

// newStoreForTest returns a migrated Store with the egress plugin over a
// fresh SQLite file.
func newStoreForTest(t *testing.T) (*records.Store, *egress.Plugin, error) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return newStore(t.Context(), db, time.Now)
}
