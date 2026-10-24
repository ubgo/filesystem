// Command records-acl shows the acl plugin of the records layer on SQLite
// and an in-memory disk: grants on a folder that reach every file in it,
// a grant on one file for a named user, levels that include the ones
// below, the "shared with" and "shared with me" listings, revoking, and
// the same checks behind the recordshttp routes through acl.Authorizer.
//
//	go run ./records-acl
//
// It prints what each step returned; main_test.go runs the same steps.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/acl"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/recordshttp"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	diskName = "docs"
	tenant   = "acme"
	// Subjects are the app's strings; the plugin never expands a user
	// into its groups.
	alice  = "user:alice"
	bob    = "user:bob"
	legal  = "group:legal"
	header = "X-User" // stands in for the app's authentication
)

// newStore wires the folders and acl plugins. WithHierarchy lets grants
// name folders: a folder grant reaches every file in the folder and its
// subfolders at the time of the check.
// ds:def id=newaclstore-w3n8k5qp
func newStore(ctx context.Context, db *sql.DB) (*records.Store, *folders.Plugin, *acl.Plugin, error) {
	fp := folders.New()
	ap := acl.New(acl.WithHierarchy(fp))
	s, err := storeWith(ctx, db, fp, ap)
	if err != nil {
		return nil, nil, nil, err
	}
	return s, fp, ap, nil
}

// storeWith is a migrated Store on db over a memory disk with signed
// links, with plugins installed.
func storeWith(ctx context.Context, db *sql.DB, plugins ...records.Plugin) (*records.Store, error) {
	signer, err := signedurl.New("https://app.example.com/files", []signedurl.Key{{ID: "k1", Secret: []byte("0123456789abcdef0123456789abcdef")}})
	if err != nil {
		return nil, err
	}
	disk, err := filesystem.NewDisk(diskName, memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(plugins...))
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// shareFolder gives the legal group write access to a folder and Bob read
// access to one file outside it.
// ds:def id=sharefolder-k9t2v6xm
func shareFolder(ctx context.Context, s *records.Store, ap *acl.Plugin, folderID, fileID string) error {
	if _, err := ap.Grant(ctx, s, tenant, acl.Grant{Subject: legal, Kind: acl.OnFolder, Resource: folderID, Level: acl.Write}); err != nil {
		return err
	}
	_, err := ap.Grant(ctx, s, tenant, acl.Grant{Subject: bob, Kind: acl.OnFile, Resource: fileID, Level: acl.Read})
	return err
}

// can asks whether a caller, speaking for every subject in who, may act
// on a file at level. Pass the user and every group it belongs to.
// ds:def id=can-c5m9w3rh
func can(ctx context.Context, s *records.Store, ap *acl.Plugin, who []string, fileID string, level acl.Level) (bool, error) {
	return ap.Can(ctx, s, tenant, who, fileID, level)
}

// subjectsOf is who a request speaks for. A real app reads its session;
// the demo trusts a header, and puts Alice in the legal group.
func subjectsOf(r *http.Request) []string {
	user := r.Header.Get(header)
	if user == "" {
		return nil
	}
	if user == alice {
		return []string{alice, legal}
	}
	return []string{user}
}

// uploaderRule decides what no grant allows: the uploader may do
// anything with their own files, everything else is refused.
func uploaderRule(s *records.Store) func(*http.Request, recordshttp.Action, recordshttp.Target) error {
	return func(r *http.Request, _ recordshttp.Action, t recordshttp.Target) error {
		if t.FileID != "" {
			if f, err := s.File(r.Context(), t.Scope, t.FileID); err == nil && f.UploadedBy == r.Header.Get(header) && f.UploadedBy != "" {
				return nil
			}
		}
		return filesystem.Errorf(filesystem.CodePermission, "not allowed")
	}
}

// newHandler serves the records routes with acl.Authorizer: reading needs
// Read; replacing, trashing, restoring and visibility or text changes
// need Write; purging needs Admin; the rest goes to uploaderRule.
// ds:def id=newaclhandler-q4h7n2zc
func newHandler(s *records.Store, ap *acl.Plugin) (http.Handler, error) {
	return recordshttp.Handler(recordshttp.Config{
		Store:     s,
		Scope:     func(*http.Request) (string, error) { return tenant, nil },
		Authorize: acl.Authorizer(ap, s, subjectsOf, uploaderRule(s)),
		Disk:      diskName,
		Policy:    upload.Policy{MaxSize: 1 << 20, Allowed: []string{"text/plain"}, Key: upload.RandomKey("docs")},
	})
}

// run performs every step and prints what it got.
func run(ctx context.Context, w io.Writer, dir string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "records.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	s, fp, ap, err := newStore(ctx, db)
	if err != nil {
		return err
	}
	contracts, err := fp.Create(ctx, s, tenant, "", "Contracts")
	if err != nil {
		return err
	}
	signed, err := fp.Create(ctx, s, tenant, contracts.ID, "Signed")
	if err != nil {
		return err
	}
	nda, err := record(ctx, s, "nda.txt", "mutual NDA")
	if err != nil {
		return err
	}
	if err := fp.Put(ctx, s, tenant, nda.ID, signed.ID); err != nil {
		return err
	}
	memo, err := record(ctx, s, "memo.txt", "lunch is at noon")
	if err != nil {
		return err
	}
	if err := shareFolder(ctx, s, ap, contracts.ID, memo.ID); err != nil {
		return err
	}
	for _, c := range []struct {
		who   []string
		file  records.File
		level acl.Level
	}{
		{[]string{alice}, nda, acl.Read}, {[]string{alice, legal}, nda, acl.Read}, {[]string{alice, legal}, nda, acl.Write},
		{[]string{alice, legal}, nda, acl.Admin}, {[]string{bob}, memo, acl.Read}, {[]string{bob}, memo, acl.Write}, {[]string{bob}, nda, acl.Read},
	} {
		ok, err := can(ctx, s, ap, c.who, c.file.ID, c.level)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%-24s %-5s %-8s %t\n", strings.Join(c.who, "+"), c.level, c.file.Filename, ok)
	}
	on, err := ap.Grants(ctx, s, tenant, acl.OnFolder, contracts.ID)
	if err != nil {
		return err
	}
	mine, err := ap.GrantsOf(ctx, s, tenant, bob)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "shared with on Contracts: %s %s; shared with bob: %d grant(s)\n", on[0].Subject, on[0].Level, len(mine))
	if err := ap.Revoke(ctx, s, tenant, acl.OnFile, memo.ID, bob); err != nil {
		return err
	}
	ok, err := can(ctx, s, ap, []string{bob}, memo.ID, acl.Read)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "after revoke, bob reads memo.txt: %t\n", ok)
	if err := overHTTP(w, s, ap, nda); err != nil {
		return err
	}
	if err := customModel(ctx, w, dir); err != nil {
		return err
	}
	return denyDemo(ctx, w, dir)
}

// overHTTP asks the records routes for the NDA as Alice (granted through
// legal) and as Bob (no grant, not the uploader).
func overHTTP(w io.Writer, s *records.Store, ap *acl.Plugin, nda records.File) error {
	h, err := newHandler(s, ap)
	if err != nil {
		return err
	}
	for _, user := range []string{alice, bob} {
		req := httptest.NewRequest(http.MethodGet, "/files/"+nda.ID, nil)
		req.Header.Set(header, user)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		fmt.Fprintf(w, "GET /files/{nda} as %s: %d\n", user, rec.Code)
	}
	return nil
}

// record stores body and records it as a file of the tenant.
func record(ctx context.Context, s *records.Store, key, body string) (records.File, error) {
	disk, err := s.Disks().Disk(diskName)
	if err != nil {
		return records.File{}, err
	}
	if _, err := disk.Write(ctx, key, strings.NewReader(body), filesystem.WriteOptions{}); err != nil {
		return records.File{}, err
	}
	return s.CreateFile(ctx, records.NewFile{Scope: tenant, Disk: diskName, Key: key})
}

func main() {
	dir, err := os.MkdirTemp("", "records-acl")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
