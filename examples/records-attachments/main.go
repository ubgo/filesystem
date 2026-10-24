// Command records-attachments shows how files are attached to the
// application's own rows: a post's gallery (many files, ordered), a
// user's avatar (a single role holding one file), reordering, detaching,
// replacing the file of a role with SetAttachment, and a file shared by
// two owners.
//
//	go run ./records-attachments
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

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	// diskName is the one disk files are stored on.
	diskName = "media"
	// tenant is the scope of every file and attachment.
	tenant = "acme"
	// ownerUser and ownerPost are the application tables files attach to.
	ownerUser = "user"
	ownerPost = "post"
	// roleGallery holds many ordered files per post.
	roleGallery = "gallery"
	// roleAvatar holds at most one file per user.
	roleAvatar = "avatar"
	// roleCover is a post's cover image, also a single role.
	roleCover = "cover"
)

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

// newStore wires a Store that knows the app's owner types and which roles
// hold one file. Attach refuses an owner type not registered here, so a
// typo is an error instead of an attachment to nothing.
// ds:def id=newstore-wknhtt4d
func newStore(ctx context.Context, db *sql.DB) (*records.Store, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithOwnerTypes(ownerUser, ownerPost),
		records.WithSingleRoles(roleAvatar, roleCover),
	)
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// store writes a small object and records it, returning the file.
// ds:def id=store-8jvff75a
func store(ctx context.Context, s *records.Store, name string) (records.File, error) {
	disk, err := s.Disks().Disk(diskName)
	if err != nil {
		return records.File{}, err
	}
	key := "images/" + name
	if _, err := disk.Write(ctx, key, bytes.NewReader([]byte(name)), filesystem.WriteOptions{}); err != nil {
		return records.File{}, err
	}
	return s.CreateFile(ctx, records.NewFile{Scope: tenant, Disk: diskName, Key: key, Filename: name})
}

// buildGallery attaches three files to a post's gallery. Each Attach adds
// the file at the last position.
// ds:def id=buildgallery-8a48cqb4
func buildGallery(ctx context.Context, s *records.Store, post records.Owner) ([]records.Attachment, error) {
	var out []records.Attachment
	for _, name := range []string{"beach.jpg", "dunes.jpg", "sunset.jpg"} {
		f, err := store(ctx, s, name)
		if err != nil {
			return nil, err
		}
		a, err := s.Attach(ctx, tenant, f.ID, post, roleGallery)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// galleryNames lists a post's gallery in position order. Attachments
// returns each attachment with its file, and leaves trashed files out.
// ds:def id=gallerynames-webyqs5u
func galleryNames(ctx context.Context, s *records.Store, post records.Owner) ([]string, error) {
	list, err := s.Attachments(ctx, tenant, post, roleGallery)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list))
	for _, af := range list {
		names = append(names, fmt.Sprintf("%d:%s", af.Attachment.Position, af.File.Filename))
	}
	return names, nil
}

// moveLastToFront reorders the gallery. Reorder takes every attachment ID
// of the owner and role that Attachments lists exactly once, in the new
// order; a stale or partial list is refused and changes nothing.
// ds:def id=movelasttofront-fmtnaguu
func moveLastToFront(ctx context.Context, s *records.Store, post records.Owner, atts []records.Attachment) error {
	last := len(atts) - 1
	ids := []string{atts[last].ID}
	for _, a := range atts[:last] {
		ids = append(ids, a.ID)
	}
	return s.Reorder(ctx, tenant, post, roleGallery, ids)
}

// setAvatar makes file the user's only avatar. The file it replaces is
// moved to the trash (PreviousTrash) unless another owner still uses it.
// ds:def id=setavatar-9td4hpat
func setAvatar(ctx context.Context, s *records.Store, user records.Owner, fileID string) (records.Attachment, error) {
	return s.SetAttachment(ctx, tenant, fileID, user, roleAvatar, records.PreviousTrash)
}

// run performs every step and prints what it got.
func run(ctx context.Context, w io.Writer, dir string) error {
	db, err := openSQLite(filepath.Join(dir, "records.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	s, err := newStore(ctx, db)
	if err != nil {
		return err
	}
	if err := galleryDemo(ctx, w, s); err != nil {
		return err
	}
	if err := avatarDemo(ctx, w, s); err != nil {
		return err
	}
	return sharedFileDemo(ctx, w, s)
}

// galleryDemo builds, reorders and trims a post's gallery.
func galleryDemo(ctx context.Context, w io.Writer, s *records.Store) error {
	post := records.Owner{Type: ownerPost, ID: "post_1"}
	atts, err := buildGallery(ctx, s, post)
	if err != nil {
		return err
	}
	names, err := galleryNames(ctx, s, post)
	fmt.Fprintln(w, "gallery:", names, err)
	if err := moveLastToFront(ctx, s, post, atts); err != nil {
		return err
	}
	names, err = galleryNames(ctx, s, post)
	fmt.Fprintln(w, "reordered:", names, err)
	err = s.Reorder(ctx, tenant, post, roleGallery, []string{atts[0].ID})
	fmt.Fprintln(w, "partial reorder:", filesystem.CodeOf(err))
	if err := s.Detach(ctx, tenant, atts[1].ID); err != nil {
		return err
	}
	names, err = galleryNames(ctx, s, post)
	fmt.Fprintln(w, "after detach:", names, err)
	_, err = s.Attach(ctx, tenant, atts[0].FileID, records.Owner{Type: "usr", ID: "1"}, roleGallery)
	fmt.Fprintln(w, "unknown owner type:", filesystem.CodeOf(err))
	return nil
}

// avatarDemo sets, refuses and replaces a single-role file.
func avatarDemo(ctx context.Context, w io.Writer, s *records.Store) error {
	user := records.Owner{Type: ownerUser, ID: "usr_1"}
	first, err := store(ctx, s, "me-2024.png")
	if err != nil {
		return err
	}
	second, err := store(ctx, s, "me-2025.png")
	if err != nil {
		return err
	}
	if _, err := setAvatar(ctx, s, user, first.ID); err != nil {
		return err
	}
	_, err = s.Attach(ctx, tenant, second.ID, user, roleAvatar)
	fmt.Fprintln(w, "second Attach to a single role:", filesystem.CodeOf(err))
	a, err := setAvatar(ctx, s, user, second.ID)
	if err != nil {
		return err
	}
	old, err := s.File(ctx, tenant, first.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "avatar is the new file: %t; previous trashed: %t\n", a.FileID == second.ID, old.DeletedAt != nil)
	return nil
}

// sharedFileDemo attaches one file to two owners, then replaces it as the
// user's avatar with PreviousPurge: the post still uses it, so it is only
// detached from the user, never purged.
// ds:def id=sharedfiledemo-u3hcfqef
func sharedFileDemo(ctx context.Context, w io.Writer, s *records.Store) error {
	user := records.Owner{Type: ownerUser, ID: "usr_2"}
	post := records.Owner{Type: ownerPost, ID: "post_2"}
	shared, err := store(ctx, s, "team.png")
	if err != nil {
		return err
	}
	if _, err := s.Attach(ctx, tenant, shared.ID, post, roleCover); err != nil {
		return err
	}
	if _, err := s.SetAttachment(ctx, tenant, shared.ID, user, roleAvatar, records.PreviousPurge); err != nil {
		return err
	}
	next, err := store(ctx, s, "solo.png")
	if err != nil {
		return err
	}
	if _, err := s.SetAttachment(ctx, tenant, next.ID, user, roleAvatar, records.PreviousPurge); err != nil {
		return err
	}
	_, err = s.File(ctx, tenant, shared.ID)
	cover, _ := s.Attachments(ctx, tenant, post, roleCover)
	fmt.Fprintf(w, "shared file kept: %t, still the post's cover: %t\n", err == nil, len(cover) == 1)
	return nil
}

func main() {
	dir, err := os.MkdirTemp("", "records-attachments")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
