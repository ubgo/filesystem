package versions_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/versions"
)

// ledger is a records.UsageHooks plugin that writes down every hold and
// release it hears of, as "+size" and "-size".
type ledger struct {
	log []string
	// holdErr and releaseErr, when set, refuse every hold or release, as
	// a quota at its limit or a failing counter would.
	holdErr, releaseErr error
}

func (l *ledger) Name() string { return "ledger" }

func (l *ledger) OnHold(_ context.Context, _ *records.Store, o records.HeldObject) error {
	if l.holdErr != nil {
		return l.holdErr
	}
	l.log = append(l.log, fmt.Sprintf("+%d %s", o.Size, o.Plugin))
	return nil
}

func (l *ledger) OnRelease(_ context.Context, _ *records.Store, o records.HeldObject) error {
	if l.releaseErr != nil {
		return l.releaseErr
	}
	l.log = append(l.log, fmt.Sprintf("-%d %s", o.Size, o.Plugin))
	return nil
}

// TestHoldAndRelease: every kept version is held, and every version that
// goes (past the cap, deleted, purged with its file) is released, so a
// usage plugin's count of held bytes always equals HeldBytes.
func TestHoldAndRelease(t *testing.T) {
	ctx := t.Context()
	e, l := newLedgerEnv(t)
	s, p := e.store, e.p
	f := e.upload(t, "1")
	other, err := s.Upload(ctx, "other", "media", strings.NewReader("elsewhere"), "o.txt", anything)
	if err != nil {
		t.Fatal(err)
	}
	e.replace(t, f.ID, "22")
	e.replace(t, f.ID, "333")
	e.replace(t, f.ID, "4444") // the cap of 2 drops "1"
	if _, err := s.Replace(ctx, "other", other.ID, strings.NewReader("x"), "", anything); err != nil {
		t.Fatal(err)
	}
	want := "+1 versions; +2 versions; -1 versions; +3 versions; +9 versions"
	if got := strings.Join(l.log, "; "); got != want {
		t.Fatalf("holds and releases:\n got %s\nwant %s", got, want)
	}
	for sc, n := range map[string]int64{scope: 5, "other": 9, "none": 0} {
		if got, err := p.HeldBytes(ctx, s, sc); err != nil || got != n {
			t.Errorf("HeldBytes(%q) = %d, %v; want %d", sc, got, err, n)
		}
	}
	list, err := p.Versions(ctx, s, scope, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	l.log = nil
	if err := p.Delete(ctx, s, scope, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Purge(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(l.log, "; "); got != "-3 versions; -2 versions" {
		t.Fatalf("releases on delete and purge = %s", got)
	}
	if got, _ := p.HeldBytes(ctx, s, scope); got != 0 {
		t.Fatalf("HeldBytes after purge = %d", got)
	}
}

// newLedgerEnv is a Store with versions (keeping 2 per file) and a ledger
// installed after it.
func newLedgerEnv(t *testing.T) (*env, *ledger) {
	t.Helper()
	db, faults := testdb.OpenFaulty(t)
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	p, err := versions.New(versions.WithKeep(2))
	if err != nil {
		t.Fatal(err)
	}
	l := &ledger{}
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(p, l))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &env{db: db, faults: faults, store: s, disk: disk, p: p}, l
}

// TestRefusedHoldAndRelease: a refused hold fails the replacement and
// keeps no version; a refused release fails the deletion, at the cap or
// by Delete, and the version stays.
func TestRefusedHoldAndRelease(t *testing.T) {
	ctx := t.Context()
	e, l := newLedgerEnv(t)
	f := e.upload(t, "1")
	refused := errors.New("refused")

	l.holdErr = refused
	if _, err := e.store.Replace(ctx, scope, f.ID, strings.NewReader("2"), "", anything); !errors.Is(err, refused) {
		t.Fatalf("replace with a refused hold = %v", err)
	}
	if got := e.content(t, f.ID); got != "1" || len(mustVersions(t, e.p, e.store, f.ID)) != 0 {
		t.Fatalf("the refused replacement left content %q and versions", got)
	}
	l.holdErr = nil

	e.replace(t, f.ID, "2")
	e.replace(t, f.ID, "3") // at the cap of 2
	l.releaseErr = refused
	if _, err := e.store.Replace(ctx, scope, f.ID, strings.NewReader("4"), "", anything); !errors.Is(err, refused) {
		t.Fatalf("replace past the cap with a refused release = %v", err)
	}
	list := mustVersions(t, e.p, e.store, f.ID)
	if err := e.p.Delete(ctx, e.store, scope, list[0].ID); !errors.Is(err, refused) {
		t.Fatalf("delete with a refused release = %v", err)
	}
	if got := len(mustVersions(t, e.p, e.store, f.ID)); got != 2 || e.content(t, f.ID) != "3" {
		t.Fatalf("refused releases changed something: %d versions, content %q", got, e.content(t, f.ID))
	}
}

// TestHeldBytesDatabaseFailure: a failing query is returned, not read as
// zero bytes.
func TestHeldBytesDatabaseFailure(t *testing.T) {
	e, _ := newLedgerEnv(t)
	e.faults.Arm(1)
	_, err := e.p.HeldBytes(t.Context(), e.store, scope)
	e.faults.Arm(0)
	if !errors.Is(err, testdb.ErrInjected) {
		t.Fatalf("HeldBytes = %v", err)
	}
}
