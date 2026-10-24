package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"legalhold/0001_legal_holds",
		"notified: counsel@acme.test held fil_",
		"trash: filesystem: permission: the file is under legal hold",
		"purge: filesystem: permission: the file is under legal hold",
		"swept 1 file(s), <nil>",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// setup returns a store with the plugin, a counter of notifications, and
// one uploaded file.
func setup(t *testing.T) (*Plugin, *records.Store, *int, records.File) {
	t.Helper()
	notified := 0
	p := New(WithNotify(func(context.Context, Hold) { notified++ }))
	store, _, err := newStore(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	f, err := store.Upload(t.Context(), scope, diskName, strings.NewReader("x\n"), "a.txt", docs)
	if err != nil {
		t.Fatal(err)
	}
	return p, store, &notified, f
}

func TestHoldRolledBackNotifiesNobody(t *testing.T) {
	p, store, notified, f := setup(t)
	ctx := t.Context()
	errAbort := errors.New("abort")
	err := store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if err := p.Hold(ctx, store, scope, f.ID, "r"); err != nil {
			return err
		}
		return errAbort
	})
	if !errors.Is(err, errAbort) || *notified != 0 {
		t.Fatalf("err %v, notified %d", err, *notified)
	}
	if held, err := p.isHeld(ctx, store, scope, f.ID); held || err != nil {
		t.Errorf("rolled-back hold kept: %v %v", held, err)
	}
	if err := p.Hold(ctx, store, scope, f.ID, "r"); err != nil || *notified != 1 {
		t.Fatalf("hold: %v, notified %d", err, *notified)
	}
	if err := p.Hold(ctx, store, scope, f.ID, "r"); filesystem.CodeOf(err) != filesystem.CodeExists {
		t.Errorf("second hold = %v", err)
	}
	if err := p.Release(ctx, store, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.Release(ctx, store, scope, f.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("second release = %v", err)
	}
}

// TestTrashedHoldFailsEverySweep pins why Hold refuses a trashed file:
// SweepTrash would report the refused purge on every run.
func TestTrashedHoldFailsEverySweep(t *testing.T) {
	p, store, _, f := setup(t)
	ctx := t.Context()
	if err := store.Trash(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.Hold(ctx, store, scope, f.ID, "r"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("hold of a trashed file = %v", err)
	}
	ins := sqlb.NewInsert(store.Dialect()).Into(Table).Row(sqlb.Set(Scope, scope), sqlb.Set(FileID, f.ID),
		sqlb.Set(Reason, "r"), sqlb.Set(HeldBy, ""), sqlb.Set(HeldAt, time.Now().UTC()))
	if _, err := store.Exec(ctx, ins); err != nil {
		t.Fatal(err)
	}
	if n, err := store.SweepTrash(ctx, time.Now().Add(time.Minute)); n != 0 || filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Errorf("sweep = %d, %v", n, err)
	}
}
