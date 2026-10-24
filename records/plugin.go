package records

import (
	"context"
)

// Plugin extends a Store. The only required method is Name; every
// capability is an optional interface the Store detects: Migrator for the
// plugin's own tables, FileHooks for the file lifecycle.
type Plugin interface {
	// Name identifies the plugin in the migration ledger and errors. It
	// must be unique and match [a-z][a-z0-9_.]*.
	Name() string
}

// Migrator is a plugin with tables of its own. Migrations must be written
// for engine (EnginePostgres, EngineMySQL, EngineSQLite) and owned by the
// plugin's name.
type Migrator interface {
	Migrations(engine string) ([]Migration, error)
}

// FileHooks lets a plugin take part in the file lifecycle. Each hook runs
// inside the operation's transaction (use Store.Conn(ctx) for statements),
// and an error aborts the operation and rolls it back. Typical uses: quota
// counters, audit rows, search indexing, refusing a purge under legal hold.
type FileHooks interface {
	// BeforeCreate runs before a file row is inserted and may change it
	// (metadata, visibility), but not its ID, Disk or Key.
	BeforeCreate(ctx context.Context, s *Store, f *File) error
	// AfterCreate runs after the row is inserted.
	AfterCreate(ctx context.Context, s *Store, f File) error
	// BeforePurge runs before a file's rows are deleted for good.
	BeforePurge(ctx context.Context, s *Store, f File) error
}

// hooks returns the installed plugins that implement FileHooks.
func (s *Store) hooks() []FileHooks {
	var out []FileHooks
	for _, p := range s.plugins {
		if h, ok := p.(FileHooks); ok {
			out = append(out, h)
		}
	}
	return out
}
