package folders_test

import (
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/internal/testdb"
)

// TestStoreClock pins the default clock: without folders.WithClock, a
// folder is stamped with the Store's clock (records.WithClock), so one
// fake clock rules the Store and the plugin alike.
func TestStoreClock(t *testing.T) {
	now := time.Date(2040, 1, 2, 3, 4, 5, 0, time.UTC)
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	p := folders.New()
	s, err := records.New(testdb.Open(t), testdb.Dialect(), mgr, records.WithClock(func() time.Time { return now }), records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	f, err := p.Create(t.Context(), s, "t", folders.RootID, "Docs")
	if err != nil {
		t.Fatal(err)
	}
	if !f.CreatedAt.Equal(now) {
		t.Fatalf("folder created at %v, want the Store's clock %v", f.CreatedAt, now)
	}
}
