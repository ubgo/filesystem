package records_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/upload"
)

// state renders every row the Store owns and the number of stored objects,
// to prove a failed operation changed nothing.
func (e *env) state(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		"SELECT " + records.ColFileID + ", CASE WHEN " + records.ColFileDeletedAt + " IS NULL THEN 'live' ELSE 'trash' END FROM " + records.TableNameFiles + " ORDER BY 1",
		"SELECT " + records.ColFileKey + ", " + records.ColFileVisibility + " FROM " + records.TableNameFiles + " ORDER BY 1",
		"SELECT " + records.ColAttachmentID + ", " + records.ColAttachmentPosition + " FROM " + records.TableNameAttachments + " ORDER BY 1",
	} {
		rows, err := e.db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, v string
			_ = rows.Scan(&id, &v)
			fmt.Fprintf(&b, "%s=%s;", id, v)
		}
		_ = rows.Close()
		b.WriteString("|")
	}
	fmt.Fprintf(&b, "objects=%d", countObjects(t, e.disk))
	// Each object's own visibility, which a failed SetVisibility must put back.
	for en, err := range e.disk.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		st, _ := e.disk.Stat(t.Context(), en.Key)
		fmt.Fprintf(&b, ";%s=%s", en.Key, st.Visibility)
	}
	return b.String()
}

// TestFaultSweep fails every database step of every Store operation in
// turn, on every engine: each run must report the injected fault and
// leave rows and objects as they were (an upload's object is deleted, a
// purge's kept), and the run after the last step must succeed.
func TestFaultSweep(t *testing.T) {
	ctx := t.Context()
	db, faults := testdb.OpenFaulty(t)
	e := newEnvOn(t, db, nil, records.WithPlugins(&recorder{}))
	post := records.Owner{Type: "post", ID: "p"}
	a, b, c := e.put(t, "t", "a.png"), e.put(t, "t", "b.png"), e.put(t, "t", "c.png")
	attA, _ := e.store.Attach(ctx, "t", a.ID, post, "g")
	attB, _ := e.store.Attach(ctx, "t", b.ID, post, "g")
	_ = e.store.Trash(ctx, "t", c.ID)
	_, _ = e.store.Attach(ctx, "t", e.put(t, "t", "old-avatar.png").ID, records.Owner{Type: "user", ID: "u"}, "avatar")
	leaving := records.Owner{Type: "user", ID: "leaving"}
	_, _ = e.store.Attach(ctx, "t", e.put(t, "t", "d.png").ID, leaving, "gallery")
	// An upload of the leaving user that was never attached.
	if _, err := e.disk.WriteBytes(ctx, "loose.png", []byte(pngData), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "loose.png", UploadedBy: "user:leaving"}); err != nil {
		t.Fatal(err)
	}
	policy := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"image/*"}}
	// A public file, whose object Trash hides and Restore shows again.
	pub := e.put(t, "t", "pub.png")
	if err := e.store.SetVisibility(ctx, "t", pub.ID, filesystem.VisibilityPublic); err != nil {
		t.Fatal(err)
	}
	// A private file, and a public object that becomes its content.
	r := e.put(t, "t", "r.png")
	if _, err := e.disk.WriteBytes(ctx, "next.png", []byte(pngData), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic}); err != nil {
		t.Fatal(err)
	}

	ops := []struct {
		name string
		run  func() error
	}{
		{"create file", func() error {
			_, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "a.png"})
			return err
		}},
		{"upload", func() error {
			_, err := e.store.Upload(ctx, "t", "media", strings.NewReader(pngData), "new.png", policy)
			return err
		}},
		{"attach", func() error { _, err := e.store.Attach(ctx, "t", a.ID, post, "cover"); return err }},
		{"reorder", func() error { return e.store.Reorder(ctx, "t", post, "g", []string{attB.ID, attA.ID}) }},
		{"detach", func() error { return e.store.Detach(ctx, "t", attA.ID) }},
		{"trash", func() error { return e.store.Trash(ctx, "t", a.ID) }},
		{"restore", func() error { return e.store.Restore(ctx, "t", a.ID) }},
		{"replace", func() error {
			_, err := e.store.Replace(ctx, "t", a.ID, strings.NewReader(pngData+"-v2"), "", policy)
			return err
		}},
		{"set attachment", func() error {
			_, err := e.store.SetAttachment(ctx, "t", a.ID, records.Owner{Type: "user", ID: "u"}, "avatar", records.PreviousPurge)
			return err
		}},
		{"purge owner", func() error { _, err := e.store.PurgeOwner(ctx, "t", leaving); return err }},
		{"purge unattached", func() error { _, err := e.store.PurgeUnattached(ctx, "t", "user:leaving"); return err }},
		{"set visibility", func() error { return e.store.SetVisibility(ctx, "t", a.ID, filesystem.VisibilityPublic) }},
		{"trash public", func() error { return e.store.Trash(ctx, "t", pub.ID) }},
		{"restore public", func() error { return e.store.Restore(ctx, "t", pub.ID) }},
		{"replace object", func() error {
			_, err := e.store.ReplaceObject(ctx, "t", r.ID, records.NewContent{Key: "next.png"})
			return err
		}},
		{"purge", func() error { return e.store.Purge(ctx, "t", b.ID) }},
		{"sweep trash", func() error { _, err := e.store.SweepTrash(ctx, time.Now().Add(time.Hour)); return err }},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			for n := 1; ; n++ {
				before := e.state(t)
				faults.Arm(n)
				err := op.run()
				faults.Arm(0)
				if err == nil {
					if n == 1 {
						t.Fatal("the operation took no database step")
					}
					return
				}
				if !errors.Is(err, testdb.ErrInjected) {
					t.Fatalf("step %d: err = %v, want the injected fault", n, err)
				}
				if after := e.state(t); after != before {
					t.Fatalf("step %d failed but changed the store:\nbefore %s\nafter  %s", n, before, after)
				}
			}
		})
	}
}

// TestMigrateFaultSweep fails every database step of a first Migrate in
// turn, each time on a fresh database: the failed run reports the fault,
// and the next run (as after a crash and restart) completes the schema.
// On MySQL, whose DDL commits as it goes, this proves a half-applied
// migration is finished rather than broken.
func TestMigrateFaultSweep(t *testing.T) {
	// One subtest per step, so each step's database and its connections
	// are released before the next (Postgres caps its clients).
	done := false
	for n := 1; !done; n++ {
		t.Run(fmt.Sprintf("step %d", n), func(t *testing.T) {
			db, faults := testdb.OpenFaulty(t)
			mgr, _ := filesystem.NewManager()
			s, err := records.New(db, testDialect(), mgr, records.WithPlugins(&recorder{}))
			if err != nil {
				t.Fatal(err)
			}
			faults.Arm(n)
			_, err = s.Migrate(t.Context())
			faults.Arm(0)
			if err == nil {
				if n == 1 {
					t.Fatal("Migrate took no database step")
				}
				done = true
				return
			}
			if !errors.Is(err, testdb.ErrInjected) {
				t.Fatalf("err = %v, want the injected fault", err)
			}
			if _, err := s.Migrate(t.Context()); err != nil {
				t.Fatalf("the Migrate after the failure: %v", err)
			}
			if _, err := s.Files(t.Context(), records.FileQuery{Scope: "t"}); err != nil {
				t.Fatalf("the schema is unusable: %v", err)
			}
		})
		if t.Failed() {
			return
		}
	}
}
