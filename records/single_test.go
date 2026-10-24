package records_test

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

func TestSingleRoles(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, records.WithSingleRoles("avatar"))
	user := records.Owner{Type: "user", ID: "u1"}
	a, b := e.put(t, "t", "a.png"), e.put(t, "t", "b.png")
	if _, err := e.store.Attach(ctx, "t", a.ID, user, "avatar"); err != nil {
		t.Fatal(err)
	}
	_, err := e.store.Attach(ctx, "t", b.ID, user, "avatar")
	if filesystem.CodeOf(err) != filesystem.CodeExists || !strings.Contains(err.Error(), "SetAttachment") {
		t.Fatalf("a second avatar = %v", err)
	}
	// Another owner, and another scope, have their own avatar.
	if _, err := e.store.Attach(ctx, "t", b.ID, records.Owner{Type: "user", ID: "u2"}, "avatar"); err != nil {
		t.Fatal(err)
	}
	if _, err := records.New(openDB(t), testDialect(), e.store.Disks(), records.WithSingleRoles("Bad Role")); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("bad role = %v", err)
	}
}

func TestSetAttachment(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, records.WithSingleRoles("avatar"))
	user := records.Owner{Type: "user", ID: "u1"}
	first, second, third, fourth := e.put(t, "t", "1.png"), e.put(t, "t", "2.png"), e.put(t, "t", "3.png"), e.put(t, "t", "4.png")
	avatar := func() string {
		list, err := e.store.Attachments(ctx, "t", user, "avatar")
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, af := range list {
			ids = append(ids, af.File.ID)
		}
		return strings.Join(ids, ",")
	}
	if _, err := e.store.SetAttachment(ctx, "t", first.ID, user, "avatar", records.PreviousPurge); err != nil || avatar() != first.ID {
		t.Fatalf("first = %v, %s", err, avatar())
	}
	if a, err := e.store.SetAttachment(ctx, "t", first.ID, user, "avatar", records.PreviousPurge); err != nil || a.FileID != first.ID {
		t.Fatalf("the same file again = %+v, %v", a, err)
	}

	// Trash: the old one goes to the trash.
	if _, err := e.store.SetAttachment(ctx, "t", second.ID, user, "avatar", records.PreviousTrash); err != nil || avatar() != second.ID {
		t.Fatalf("second = %v, %s", err, avatar())
	}
	if f, _ := e.store.File(ctx, "t", first.ID); f.DeletedAt == nil {
		t.Fatal("the previous avatar was not trashed")
	}

	// Purge, except a file someone else still uses.
	if _, err := e.store.Attach(ctx, "t", second.ID, records.Owner{Type: "post", ID: "p1"}, "cover"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.SetAttachment(ctx, "t", third.ID, user, "avatar", records.PreviousPurge); err != nil {
		t.Fatal(err)
	}
	if f, err := e.store.File(ctx, "t", second.ID); err != nil || f.DeletedAt != nil {
		t.Fatalf("a shared file was disposed of: %+v, %v", f, err)
	}
	if _, err := e.store.SetAttachment(ctx, "t", fourth.ID, user, "avatar", records.PreviousPurge); err != nil {
		t.Fatal(err)
	}
	_, err := e.store.File(ctx, "t", third.ID)
	wantCode(t, err, filesystem.CodeNotFound)
	if ok, _ := e.disk.Exists(ctx, third.Key); ok {
		t.Fatal("the purged avatar's object remains")
	}

	// Keep: only detached. A trashed attached file is detached too.
	_ = e.store.Trash(ctx, "t", fourth.ID)
	fifth := e.put(t, "t", "5.png")
	if _, err := e.store.SetAttachment(ctx, "t", fifth.ID, user, "avatar", records.PreviousKeep); err != nil || avatar() != fifth.ID {
		t.Fatalf("fifth = %v, %s", err, avatar())
	}
	if f, err := e.store.File(ctx, "t", fourth.ID); err != nil || f.DeletedAt == nil {
		t.Fatalf("keep changed the file: %+v, %v", f, err)
	}

	// A plain role is replaced whole.
	post := records.Owner{Type: "post", ID: "p9"}
	for _, f := range []records.File{first, second} {
		_, _ = e.store.Attach(ctx, "t", f.ID, post, "gallery")
	}
	if _, err := e.store.SetAttachment(ctx, "t", fifth.ID, post, "gallery", records.PreviousKeep); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.store.Attachments(ctx, "t", post, "gallery"); len(list) != 1 || list[0].File.ID != fifth.ID {
		t.Fatalf("gallery = %+v", list)
	}

	_, err = e.store.SetAttachment(ctx, "t", fifth.ID, user, "avatar", "shred")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = e.store.SetAttachment(ctx, "t", fifth.ID, records.Owner{Type: "nope", ID: "x"}, "avatar", records.PreviousKeep)
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if len(records.PreviousValues) != 3 {
		t.Fatal("PreviousValues")
	}
}

// TestSetAttachmentConcurrent races writers for one owner's avatar: the
// database admits one attachment, whatever the interleaving.
func TestSetAttachmentConcurrent(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, records.WithSingleRoles("avatar"))
	user := records.Owner{Type: "user", ID: "u1"}
	const writers = 8
	files := make([]records.File, writers)
	for i := range files {
		files[i] = e.put(t, "t", fmt.Sprintf("%d.png", i))
	}
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Go(func() {
			_, errs[i] = e.store.SetAttachment(ctx, "t", files[i].ID, user, "avatar", records.PreviousKeep)
		})
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		if err == nil {
			won++
		}
	}
	list, err := e.store.Attachments(ctx, "t", user, "avatar")
	if err != nil || len(list) != 1 || won == 0 {
		t.Fatalf("%d attachments after %d winners: %v %v", len(list), won, errs, err)
	}
}

func TestPurgeOwner(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, records.WithSingleRoles("avatar"))
	user, other := records.Owner{Type: "user", ID: "u1"}, records.Owner{Type: "user", ID: "u2"}
	avatar, photo, shared, trashed := e.put(t, "t", "a.png"), e.put(t, "t", "b.png"), e.put(t, "t", "c.png"), e.put(t, "t", "d.png")
	_, _ = e.store.Attach(ctx, "t", avatar.ID, user, "avatar")
	_, _ = e.store.Attach(ctx, "t", avatar.ID, user, "gallery") // the same file under two roles
	_, _ = e.store.Attach(ctx, "t", photo.ID, user, "gallery")
	_, _ = e.store.Attach(ctx, "t", photo.ID, records.Owner{Type: "post", ID: "p1"}, "gallery") // also on a post
	_, _ = e.store.Attach(ctx, "t", shared.ID, user, "gallery")
	_, _ = e.store.Attach(ctx, "t", shared.ID, other, "gallery")
	_, _ = e.store.Attach(ctx, "t", trashed.ID, user, "gallery")
	_ = e.store.Trash(ctx, "t", trashed.ID)

	n, err := e.store.PurgeOwner(ctx, "t", user)
	if err != nil || n != 2 {
		t.Fatalf("PurgeOwner = %d, %v", n, err)
	}
	for _, f := range []records.File{avatar, trashed} {
		if _, err := e.store.File(ctx, "t", f.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
			t.Errorf("%s survived: %v", f.Key, err)
		}
		if ok, _ := e.disk.Exists(ctx, f.Key); ok {
			t.Errorf("%s object survived", f.Key)
		}
	}
	for _, f := range []records.File{photo, shared} {
		if _, err := e.store.File(ctx, "t", f.ID); err != nil {
			t.Errorf("a file another owner uses was purged: %s", f.Key)
		}
	}
	if list, _ := e.store.Attachments(ctx, "t", user, "gallery"); len(list) != 0 {
		t.Fatalf("attachments left: %+v", list)
	}
	if list, _ := e.store.Attachments(ctx, "t", other, "gallery"); len(list) != 1 {
		t.Fatal("the other owner lost its attachment")
	}
	if n, err := e.store.PurgeOwner(ctx, "t", user); err != nil || n != 0 {
		t.Fatalf("again = %d, %v", n, err)
	}
	_, err = e.store.PurgeOwner(ctx, "t", records.Owner{Type: "nope", ID: "x"})
	wantCode(t, err, filesystem.CodeInvalidArgument)
}

func TestPurgeUnattached(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	ana, bob := records.WithActor(ctx, "user:ana"), records.WithActor(ctx, "user:bob")
	put := func(ctx context.Context, key string) records.File {
		t.Helper()
		if _, err := e.disk.WriteBytes(ctx, key, []byte(pngData), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		f, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: key})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	loose, detached, trashed, used := put(ana, "loose.png"), put(ana, "detached.png"), put(ana, "trashed.png"), put(ana, "used.png")
	usedTrashed, bobs, anonymous := put(ana, "used-trashed.png"), put(bob, "bob.png"), put(ctx, "anon.png")
	// The same uploader's unattached file in another scope.
	if _, err := e.store.CreateFile(ana, records.NewFile{Scope: "u", Disk: "media", Key: "anon.png"}); err != nil {
		t.Fatal(err)
	}
	post := records.Owner{Type: "post", ID: "p1"}
	att, _ := e.store.Attach(ctx, "t", detached.ID, post, "gallery")
	_ = e.store.Detach(ctx, "t", att.ID)
	_ = e.store.Trash(ctx, "t", trashed.ID)
	_, _ = e.store.Attach(ctx, "t", used.ID, post, "gallery")
	_, _ = e.store.Attach(ctx, "t", usedTrashed.ID, post, "gallery")
	_ = e.store.Trash(ctx, "t", usedTrashed.ID)

	// A filter narrows the purge: only files created after "detached".
	n, err := e.store.PurgeUnattached(ctx, "t", "user:ana", sqlb.Greater(records.FileCreatedAt, detached.CreatedAt))
	if err != nil || n != 1 {
		t.Fatalf("filtered PurgeUnattached = %d, %v", n, err)
	}
	n, err = e.store.PurgeUnattached(ctx, "t", "user:ana")
	if err != nil || n != 2 {
		t.Fatalf("PurgeUnattached = %d, %v", n, err)
	}
	for _, f := range []records.File{loose, detached, trashed} {
		if _, err := e.store.File(ctx, "t", f.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
			t.Errorf("%s survived: %v", f.Key, err)
		}
		if ok, _ := e.disk.Exists(ctx, f.Key); ok {
			t.Errorf("%s object survived", f.Key)
		}
	}
	for _, f := range []records.File{used, usedTrashed, bobs, anonymous} {
		if _, err := e.store.File(ctx, "t", f.ID); err != nil {
			t.Errorf("%s was purged: %v", f.Key, err)
		}
	}
	if page, _ := e.store.Files(ctx, records.FileQuery{Scope: "u"}); len(page.Files) != 1 {
		t.Fatal("a file of another scope was purged")
	}
	if n, err := e.store.PurgeUnattached(ctx, "t", "user:ana"); err != nil || n != 0 {
		t.Fatalf("again = %d, %v", n, err)
	}
	_, err = e.store.PurgeUnattached(ctx, "t", "")
	wantCode(t, err, filesystem.CodeInvalidArgument)
}

// TestPurgeUnattachedRacingAttach holds an Attach's transaction open while
// PurgeUnattached runs. The purge must wait for it and then keep the file,
// which is attached by then; without the file lock, on Postgres and MySQL,
// it purges the file and the attachment commits pointing at nothing.
// (SQLite has one writer, so there the purge waits either way.)
func TestPurgeUnattachedRacingAttach(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	ana := records.WithActor(ctx, "user:ana")
	if _, err := e.disk.WriteBytes(ctx, "a.png", []byte(pngData), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.CreateFile(ana, records.NewFile{Scope: "t", Disk: "media", Key: "a.png"})
	if err != nil {
		t.Fatal(err)
	}
	post := records.Owner{Type: "post", ID: "p1"}
	held, release := make(chan struct{}), make(chan struct{})
	attached := make(chan error, 1)
	go func() {
		attached <- e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
			_, err := e.store.Attach(ctx, "t", f.ID, post, "gallery")
			close(held)
			if err != nil {
				return err
			}
			<-release
			return nil
		})
	}()
	<-held
	purged := make(chan error, 1)
	var n int
	go func() {
		var err error
		n, err = e.store.PurgeUnattached(ctx, "t", "user:ana")
		purged <- err
	}()
	// Without the lock the purge finishes at once; give it the time.
	var purgeErr error
	finished := false
	select {
	case purgeErr = <-purged:
		finished = true
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-attached; err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !finished {
		purgeErr = <-purged
	}
	if purgeErr != nil || n != 0 {
		t.Fatalf("PurgeUnattached = %d, %v; want the newly attached file kept", n, purgeErr)
	}
	if list, err := e.store.Attachments(ctx, "t", post, "gallery"); err != nil || len(list) != 1 {
		t.Fatalf("attachments = %v, %v", list, err)
	}
}

// changer runs fn from BeforePurge of the file keyed key, inside the
// purge's transaction: a change landing between PurgeUnattached's listing
// and its per-file check.
type changer struct {
	key string
	fn  func(ctx context.Context, s *records.Store) error
}

func (c *changer) Name() string { return "changer" }
func (c *changer) BeforeCreate(context.Context, *records.Store, *records.File) error {
	return nil
}
func (c *changer) AfterCreate(context.Context, *records.Store, records.File) error { return nil }
func (c *changer) BeforePurge(ctx context.Context, s *records.Store, f records.File) error {
	if f.Key == c.key && c.fn != nil {
		fn := c.fn
		c.fn = nil
		return fn(ctx, s)
	}
	return nil
}

func TestPurgeUnattachedSeesChangesMidway(t *testing.T) {
	ctx := t.Context()
	c := &changer{}
	e := newEnv(t, nil, records.WithPlugins(c))
	ana := records.WithActor(ctx, "user:ana")
	record := func(key string) records.File {
		t.Helper()
		if _, err := e.disk.WriteBytes(ctx, key, []byte(pngData), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		f, err := e.store.CreateFile(ana, records.NewFile{Scope: "t", Disk: "media", Key: key})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	files := []records.File{record("a.png"), record("b.png"), record("c.png")}
	slices.SortFunc(files, func(x, y records.File) int { return strings.Compare(x.ID, y.ID) })
	first, gone, kept := files[0], files[1], files[2]
	post := records.Owner{Type: "post", ID: "p1"}
	// Purging the first file purges the second and attaches the third, as
	// other requests could while the call runs.
	c.key = first.Key
	c.fn = func(ctx context.Context, s *records.Store) error {
		if err := s.Purge(ctx, "t", gone.ID); err != nil {
			return err
		}
		_, err := s.Attach(ctx, "t", kept.ID, post, "gallery")
		return err
	}
	n, err := e.store.PurgeUnattached(ctx, "t", "user:ana")
	if err != nil || n != 1 {
		t.Fatalf("PurgeUnattached = %d, %v; want only the first counted", n, err)
	}
	if _, err := e.store.File(ctx, "t", kept.ID); err != nil {
		t.Fatalf("the file attached midway was purged: %v", err)
	}
}
