package locks_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/locks"
)

// TestStoreActorFunc: without locks.WithOwnerFunc a request's owner is the
// Store's actor (records.WithActorFunc); the plugin's own option wins.
func TestStoreActorFunc(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    []locks.Option
		refused bool
	}{
		{"store's", nil, false},
		{"plugin's wins", []locks.Option{locks.WithOwnerFunc(func(context.Context) string { return "plugin:bob" })}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disk, _ := filesystem.NewDisk("media", memory.New())
			mgr, _ := filesystem.NewManager(disk)
			p := locks.New(tc.opts...)
			s, err := records.New(testdb.Open(t), testdb.Dialect(), mgr, records.WithPlugins(p),
				records.WithActorFunc(func(context.Context) string { return "store:alice" }))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Migrate(t.Context()); err != nil {
				t.Fatal(err)
			}
			f, err := s.Upload(t.Context(), "t", "media", strings.NewReader("v1"), "doc.txt", anything)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Lock(t.Context(), s, "t", f.ID, "store:alice", time.Hour); err != nil {
				t.Fatal(err)
			}
			// No records.WithActor on ctx: only the Store's actor func names
			// the owner.
			_, err = s.Replace(t.Context(), "t", f.ID, strings.NewReader("v2"), "", anything)
			if got := errors.Is(err, locks.ErrLocked); got != tc.refused || (!tc.refused && err != nil) {
				t.Fatalf("Replace = %v, want refused %v", err, tc.refused)
			}
		})
	}
}
