package share_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/share"
)

// TestQuarantinedFileLink: a quarantined file cannot be shared, and a
// link made before its file was quarantined answers its visitors with
// the uniform not-found, spending nothing, until the scan passes.
func TestQuarantinedFileLink(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, true)
	sh, err := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.SetStatus(ctx, "t", e.file.ID, records.StatusQuarantined, "rescan"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour}); !errors.Is(err, records.ErrQuarantined) {
		t.Fatalf("sharing a quarantined file = %v", err)
	}
	_, err = e.plugin.Resolve(ctx, e.store, sh.Token)
	gone(t, err)
	_, err = e.plugin.Check(ctx, e.store, sh.Token)
	gone(t, err)
	h := share.Handler(e.store, e.plugin)
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		if rec := serve(h, m, "/"+sh.Token, nil, nil); rec.Code != http.StatusNotFound || rec.Header().Get("Location") != "" {
			t.Fatalf("%s of a quarantined file's link = %d %v", m, rec.Code, rec.Header())
		}
	}
	if n := e.downloads(t, sh.ID); n != 0 {
		t.Fatalf("refused visits spent %d downloads", n)
	}
	if _, err := e.store.PassScan(ctx, "t", e.file.ID); err != nil {
		t.Fatal(err)
	}
	if rec := serve(h, http.MethodGet, "/"+sh.Token, nil, nil); rec.Code != http.StatusFound {
		t.Fatalf("GET after the scan = %d", rec.Code)
	}
}

// TestQuarantinedInFolderLink: a folder link does not list a quarantined
// file, and refuses to serve it, while serving the folder's other files.
func TestQuarantinedInFolderLink(t *testing.T) {
	ctx := t.Context()
	u := newSuite(t, 0)
	dir, err := u.folders.Create(ctx, u.store, "t", folders.RootID, "Inbox")
	if err != nil {
		t.Fatal(err)
	}
	ok := u.file(t, "ok.txt")
	bad, err := u.store.Upload(ctx, "t", "media", strings.NewReader("unscanned"), "bad.txt", anyText, records.Quarantine())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []records.File{ok, bad} {
		if err := u.folders.Put(ctx, u.store, "t", f.ID, dir.ID); err != nil {
			t.Fatal(err)
		}
	}
	sh, err := u.plugin.CreateFor(ctx, u.store, "t", share.Target{Kind: folders.KindFolder, Ref: dir.ID}, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	files, _, err := u.plugin.List(ctx, u.store, sh.Token)
	if err != nil || len(files) != 1 || files[0].ID != ok.ID {
		t.Fatalf("List = %+v, %v", files, err)
	}
	h := share.Handler(u.store, u.plugin)
	rec := serve(h, http.MethodGet, "/"+sh.Token, nil, nil)
	var listed []share.ListedFile
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || len(listed) != 1 {
		t.Fatalf("listed %s", rec.Body)
	}
	_, err = u.plugin.ResolveFile(ctx, u.store, sh.Token, bad.ID)
	gone(t, err)
	_, err = u.plugin.CheckFile(ctx, u.store, sh.Token, bad.ID)
	gone(t, err)
	if rec := serve(h, http.MethodGet, "/"+sh.Token+"/"+ok.ID, nil, nil); rec.Code != http.StatusFound {
		t.Fatalf("download of the clean file = %d", rec.Code)
	}
	if n := u.uses(t, sh); n != 1 {
		t.Fatalf("uses = %d, want 1", n)
	}
}

// TestQuarantinedDeposits: with WithQuarantinedDeposits a file uploaded
// through a request link is recorded quarantined; without it, ready.
func TestQuarantinedDeposits(t *testing.T) {
	ctx := t.Context()
	for _, quarantine := range []bool{false, true} {
		db, _ := testdb.OpenFaulty(t)
		disk, _ := filesystem.NewDisk("media", memory.New())
		mgr, _ := filesystem.NewManager(disk)
		var opts []share.Option
		if quarantine {
			opts = append(opts, share.WithQuarantinedDeposits())
		}
		sp := share.New(append(opts, share.WithCollections(share.Owners{}))...)
		s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(sp), records.WithOwnerTypes("post"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		post := records.Owner{Type: "post", ID: "p1"}
		req, err := sp.CreateRequest(ctx, s, "t", share.Target{Kind: share.KindOwner, Ref: share.OwnerRef(post, "inbox")}, share.Options{TTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		f, err := sp.Deposit(ctx, s, req.Token, "media", strings.NewReader("from a stranger"), "d.txt", anyText)
		if err != nil || f.Quarantined() != quarantine {
			t.Fatalf("Deposit (quarantine %v) = %+v, %v", quarantine, f, err)
		}
	}
}
