// Command records-replace shows how a recorded file changes without
// changing its ID: new content (Replace, ReplaceObject) and a new
// visibility (SetVisibility), on one disk that changes access in place,
// and on a public and a private disk with WithVisibilityDisks, where the
// object moves between them.
//
//	go run ./records-replace
package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	// mediaDisk is the single disk of the in-place store.
	mediaDisk = "media"
	// publicDisk and privateDisk are the two disks of the routed store:
	// a bucket behind a CDN, and one reached only through signed URLs.
	publicDisk  = "public"
	privateDisk = "private"
	// tenant is the scope of every file.
	tenant = "acme"
	// ownerDoc is the application table files attach to.
	ownerDoc = "doc"
	// roleBody is the role of a document's file.
	roleBody = "body"
	// signBase is where a real app mounts fshttp.Signed.
	signBase = "https://app.example.com/files"
	// cdnBase is the public URL prefix of the public disk.
	cdnBase = "https://cdn.example.com"
	// maxUpload caps one upload.
	maxUpload = 1 << 20
	// linkTTL is how long a signed link works.
	linkTTL = 5 * time.Minute
)

// signingSecret signs URLs; a real app loads it from its secret store.
var signingSecret = []byte("0123456789abcdef0123456789abcdef")

// textPolicy accepts plain text with a random key under docs/.
var textPolicy = upload.Policy{MaxSize: maxUpload, Allowed: []string{"text/plain"}, Key: upload.RandomKey("docs")}

// openSQLite opens a SQLite database file with one connection (SQLite has
// one writer) and a busy timeout.
func openSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// newDisk builds a memory disk that signs URLs and has public URLs.
// ds:def id=newdisk-p28hp9gs
func newDisk(name string) (*filesystem.Disk, error) {
	signer, err := signedurl.New(signBase, []signedurl.Key{{ID: "k1", Secret: signingSecret}})
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk(name, memory.New(), filesystem.WithURLSigner(signer), filesystem.WithPublicURL(cdnBase+"/"+name))
}

// newStore wires and migrates a Store over the given disks.
func newStore(ctx context.Context, db *sql.DB, disks []*filesystem.Disk, opts ...records.Option) (*records.Store, error) {
	m, err := filesystem.NewManager(disks...)
	if err != nil {
		return nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, m, append(opts, records.WithOwnerTypes(ownerDoc))...)
	if err != nil {
		return nil, err
	}
	_, err = s.Migrate(ctx)
	return s, err
}

// newRoutedStore keeps public files on the public disk and private ones
// on the private disk. Upload with an empty disk name chooses by the
// policy's visibility; SetVisibility moves the object across.
// ds:def id=newroutedstore-n9kscxvq
func newRoutedStore(ctx context.Context, db *sql.DB) (*records.Store, error) {
	pub, err := newDisk(publicDisk)
	if err != nil {
		return nil, err
	}
	priv, err := newDisk(privateDisk)
	if err != nil {
		return nil, err
	}
	return newStore(ctx, db, []*filesystem.Disk{pub, priv}, records.WithVisibilityDisks(publicDisk, privateDisk))
}

// replaceBody uploads new content for a file. The file keeps its ID and
// attachments; an empty filename keeps the display name; the new object
// gets the file's visibility, whatever the policy says. The old object is
// deleted after the commit.
// ds:def id=replacebody-4j3zgybf
func replaceBody(ctx context.Context, s *records.Store, id, text string) (records.File, error) {
	return s.Replace(ctx, tenant, id, strings.NewReader(text), "", textPolicy)
}

// replaceWithObject makes an object already on the file's disk its new
// content: the path for bytes produced elsewhere (a worker, a presigned
// upload). The object must exist and differ from the current one.
// ds:def id=replacewithobject-w53xbxkm
func replaceWithObject(ctx context.Context, s *records.Store, f records.File, key, text string) (records.File, error) {
	disk, err := s.Disks().Disk(f.Disk)
	if err != nil {
		return records.File{}, err
	}
	if _, err := disk.Write(ctx, key, bytes.NewReader([]byte(text)), filesystem.WriteOptions{}); err != nil {
		return records.File{}, err
	}
	return s.ReplaceObject(ctx, tenant, f.ID, records.NewContent{Key: key, Filename: "notes-v3.txt"})
}

// publish makes a file public and returns its link: a CDN URL once
// public, where before it was a signed URL.
// ds:def id=publish-4e2p57p2
func publish(ctx context.Context, s *records.Store, id string) (string, error) {
	if err := s.SetVisibility(ctx, tenant, id, filesystem.VisibilityPublic); err != nil {
		return "", err
	}
	f, err := s.File(ctx, tenant, id)
	if err != nil {
		return "", err
	}
	return s.URL(ctx, f, linkTTL)
}

func main() {
	dir, err := os.MkdirTemp("", "records-replace")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}

// run performs both demos, each with its own database.
func run(ctx context.Context, w io.Writer, dir string) error {
	for i, demo := range []func(context.Context, io.Writer, *sql.DB) error{inPlaceDemo, routedDemo, unchangedDemo} {
		db, err := openSQLite(filepath.Join(dir, fmt.Sprintf("records-%d.db", i)))
		if err != nil {
			return err
		}
		err = demo(ctx, w, db)
		_ = db.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// inPlaceDemo replaces content and visibility on one disk.
func inPlaceDemo(ctx context.Context, w io.Writer, db *sql.DB) error {
	media, err := newDisk(mediaDisk)
	if err != nil {
		return err
	}
	s, err := newStore(ctx, db, []*filesystem.Disk{media})
	if err != nil {
		return err
	}
	f, err := s.Upload(ctx, tenant, mediaDisk, strings.NewReader("v1"), "notes.txt", textPolicy)
	if err != nil {
		return err
	}
	if _, err := s.Attach(ctx, tenant, f.ID, records.Owner{Type: ownerDoc, ID: "doc_1"}, roleBody); err != nil {
		return err
	}
	v2, err := replaceBody(ctx, s, f.ID, "version two")
	if err != nil {
		return err
	}
	oldKept, _ := media.Exists(ctx, f.Key)
	fmt.Fprintf(w, "Replace: same ID %t, name %q, size %d, old object kept %t\n", v2.ID == f.ID, v2.Filename, v2.Size, oldKept)
	v3, err := replaceWithObject(ctx, s, v2, "docs/v3.txt", "third")
	if err != nil {
		return err
	}
	_, err = s.ReplaceObject(ctx, tenant, f.ID, records.NewContent{Key: v3.Key})
	fmt.Fprintf(w, "ReplaceObject: name %q; same key again: %s\n", v3.Filename, filesystem.CodeOf(err))
	link, err := publish(ctx, s, f.ID)
	if err != nil {
		return err
	}
	e, err := media.Stat(ctx, v3.Key)
	fmt.Fprintf(w, "SetVisibility in place: object is %s, link %s %v\n", e.Visibility, link, err)
	return nil
}

// routedDemo uploads to the private disk and publishes to the public one.
func routedDemo(ctx context.Context, w io.Writer, db *sql.DB) error {
	s, err := newRoutedStore(ctx, db)
	if err != nil {
		return err
	}
	f, err := s.Upload(ctx, tenant, "", strings.NewReader("draft"), "draft.txt", textPolicy)
	if err != nil {
		return err
	}
	before, err := s.URL(ctx, f, linkTTL)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "routed upload: disk %s, link %s…\n", f.Disk, before[:len(signBase)])
	link, err := publish(ctx, s, f.ID)
	if err != nil {
		return err
	}
	moved, err := s.File(ctx, tenant, f.ID)
	if err != nil {
		return err
	}
	priv, _ := s.Disks().Disk(privateDisk)
	left, _ := priv.Exists(ctx, f.Key)
	fmt.Fprintf(w, "published: disk %s, link %s, private copy left %t\n", moved.Disk, link, left)
	return nil
}
