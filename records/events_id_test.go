package records_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// TestEventIDs: the Store gives every event a random ID, keeps the one a
// plugin sets (a deterministic ID for a fact it may report twice), and
// refuses one wider than an outbox's key column.
func TestEventIDs(t *testing.T) {
	ctx := t.Context()
	r := &recorder{}
	e := newEnv(t, nil, records.WithPlugins(r, named("folders")))
	emit := func(id string) error {
		return e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
			return e.store.Emit(ctx, named("folders"), records.Event{ID: id, Kind: "folders.moved", Scope: "t"})
		})
	}
	if err := emit(""); err != nil {
		t.Fatal(err)
	}
	if err := emit(""); err != nil {
		t.Fatal(err)
	}
	if err := emit("fld_move_1"); err != nil {
		t.Fatal(err)
	}
	ids := []string{r.events[0].ID, r.events[1].ID, r.events[2].ID}
	if !strings.HasPrefix(ids[0], records.IDPrefixEvent) || ids[0] == ids[1] || ids[2] != "fld_move_1" {
		t.Fatalf("ids = %q", ids)
	}
	if err := emit(strings.Repeat("x", records.MaxEventIDBytes+1)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || len(r.events) != 3 {
		t.Fatalf("oversized id = %v, events %d", err, len(r.events))
	}
}
