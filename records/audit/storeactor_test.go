package audit_test

import (
	"context"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/audit"
	"github.com/ubgo/filesystem/records/internal/testdb"
)

// TestStoreActorFunc: without audit.WithActorFunc the plugin logs the
// Store's actor (records.WithActorFunc); the plugin's own option wins.
func TestStoreActorFunc(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []audit.Option
		want string
	}{
		{"store's", nil, "store:alice"},
		{"plugin's wins", []audit.Option{audit.WithActorFunc(func(context.Context) string { return "plugin:bob" })}, "plugin:bob"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disk, _ := filesystem.NewDisk("media", memory.New())
			mgr, _ := filesystem.NewManager(disk)
			p := audit.New(tc.opts...)
			s, err := records.New(testdb.Open(t), testdb.Dialect(), mgr, records.WithPlugins(p),
				records.WithActorFunc(func(context.Context) string { return "store:alice" }))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Migrate(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := disk.WriteBytes(t.Context(), "a", []byte("a"), filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: "a"}); err != nil {
				t.Fatal(err)
			}
			page, err := p.Entries(t.Context(), s, audit.Query{Scope: scope})
			if err != nil || len(page.Entries) != 1 || page.Entries[0].Actor != tc.want {
				t.Fatalf("entries = %+v, %v; want actor %q", page.Entries, err, tc.want)
			}
		})
	}
}
