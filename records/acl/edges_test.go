package acl_test

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/acl"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/recordshttp"
)

// faultyEnv is env with its database's Faults switch and the raw handle,
// for the tests that fail database steps.
type faultyEnv struct {
	env
	db     *sql.DB
	faults *testdb.Faults
}

func newFaultyEnv(t *testing.T) *faultyEnv {
	t.Helper()
	db, faults := testdb.OpenFaulty(t)
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	fp := folders.New()
	ap := acl.New(acl.WithHierarchy(fp))
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(fp, ap))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &faultyEnv{env: env{store: s, acl: ap, folders: fp}, db: db, faults: faults}
}

func wantCode(t *testing.T, err error, code filesystem.Code) {
	t.Helper()
	if filesystem.CodeOf(err) != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
}

// TestRefusals: unknown levels, folder checks without a hierarchy,
// grants on a trashed file or on the root, and checks on unknown files
// or folders are refused with the documented codes.
func TestRefusals(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	f := e.file(t, "a.txt")
	_, err := e.acl.Can(ctx, e.store, "t", []string{"u"}, f.ID, "owner")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = e.acl.Can(ctx, e.store, "t", []string{"u"}, "fil_missing", acl.Read)
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = e.acl.CanFolder(ctx, e.store, "t", []string{"u"}, "fld_x", "owner")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = e.acl.CanFolder(ctx, e.store, "t", []string{"u"}, "fld_x", acl.Read)
	wantCode(t, err, filesystem.CodeInvalidArgument) // no hierarchy

	if err := e.store.Trash(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e.acl.Grant(ctx, e.store, "t", acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: acl.Read})
	wantCode(t, err, filesystem.CodeNotFound)

	h := newEnv(t, withFolders)
	_, err = h.acl.Grant(ctx, h.store, "t", acl.Grant{Subject: "u", Kind: acl.OnFolder, Resource: "", Level: acl.Read})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = h.acl.CanFolder(ctx, h.store, "t", []string{"u"}, "fld_missing", acl.Read)
	wantCode(t, err, filesystem.CodeNotFound)
}

// TestAuthorizerReturnsLookupFailures: a request on a file the grants
// cannot be checked for (it does not exist) is refused with the lookup's
// error, never passed on to next as if no grant applied.
func TestAuthorizerReturnsLookupFailures(t *testing.T) {
	e := newEnv(t)
	authz := acl.Authorizer(e.acl, e.store, func(*http.Request) []string { return []string{"u"} }, nil)
	err := authz(httptest.NewRequest("GET", "/", nil), recordshttp.ActionRead, recordshttp.Target{Scope: "t", FileID: "fil_missing"})
	wantCode(t, err, filesystem.CodeNotFound)
}

// TestConcurrentGrantIsRetryable: a grant of the same subject on the same
// resource that commits between Grant's delete and insert makes the
// insert a unique violation, reported as CodeUnavailable (retry), not as
// a failure of the caller's request. A SQLite trigger plays the
// concurrent grant.
func TestConcurrentGrantIsRetryable(t *testing.T) {
	testdb.OnlySQLite(t)
	ctx := t.Context()
	e := newFaultyEnv(t)
	f := e.file(t, "a.txt")
	testdb.Exec(t, e.db, "CREATE TRIGGER racer BEFORE INSERT ON "+acl.TableNameGrants+" WHEN NEW."+acl.ColID+" != 'racer' BEGIN "+
		"INSERT INTO "+acl.TableNameGrants+" ("+acl.ColID+", "+acl.ColScope+", "+acl.ColSubject+", "+acl.ColKind+", "+acl.ColResource+", "+acl.ColLevel+", "+acl.ColCreatedAt+") "+
		"VALUES ('racer', NEW."+acl.ColScope+", NEW."+acl.ColSubject+", NEW."+acl.ColKind+", NEW."+acl.ColResource+", NEW."+acl.ColLevel+", NEW."+acl.ColCreatedAt+"); END")
	_, err := e.acl.Grant(ctx, e.store, "t", acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: acl.Read})
	wantCode(t, err, filesystem.CodeUnavailable)
}

// TestFaultSweep fails every database step of Grant, Can (through the
// folder chain), CanFolder and Revoke in turn: each failure is the
// injected fault, returned, and the run after the sweep succeeds.
func TestFaultSweep(t *testing.T) {
	ctx := t.Context()
	e := newFaultyEnv(t)
	f := e.file(t, "a.txt")
	dir, err := e.folders.Create(ctx, e.store, "t", folders.RootID, "docs")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.folders.Put(ctx, e.store, "t", f.ID, dir.ID); err != nil {
		t.Fatal(err)
	}
	grant := acl.Grant{Subject: "u", Kind: acl.OnFolder, Resource: dir.ID, Level: acl.Write}
	ops := []struct {
		name string
		run  func() error
	}{
		{"grant", func() error { _, err := e.acl.Grant(ctx, e.store, "t", grant); return err }},
		{"can", func() error {
			ok, err := e.acl.Can(ctx, e.store, "t", []string{"u"}, f.ID, acl.Write)
			if err == nil && !ok {
				t.Fatal("the folder grant did not reach its file")
			}
			return err
		}},
		{"can folder", func() error {
			_, err := e.acl.CanFolder(ctx, e.store, "t", []string{"u"}, dir.ID, acl.Read)
			return err
		}},
		{"revoke", func() error { return e.acl.Revoke(ctx, e.store, "t", acl.OnFolder, dir.ID, "u") }},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			for n := 1; ; n++ {
				e.faults.Arm(n)
				err := op.run()
				e.faults.Arm(0)
				if err == nil {
					return
				}
				if !errors.Is(err, testdb.ErrInjected) {
					t.Fatalf("step %d: %v", n, err)
				}
			}
		})
	}
	if ok, err := e.acl.CanFolder(ctx, e.store, "t", []string{"u"}, dir.ID, acl.Read); err != nil || ok {
		t.Fatalf("after the revoke CanFolder = %v, %v", ok, err)
	}
}
