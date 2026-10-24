package records_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

// recorder logs every event and refuses the kind in refuse.
type recorder struct {
	events []records.Event
	refuse records.EventKind
}

func (r *recorder) Name() string { return "recorder" }

func (r *recorder) OnEvent(_ context.Context, _ *records.Store, e records.Event) error {
	if e.Kind == r.refuse {
		return errors.New("refused " + string(e.Kind))
	}
	r.events = append(r.events, e)
	return nil
}

// log renders the events compactly, with IDs replaced by what they name.
func (r *recorder) log(names map[string]string) string {
	var out []string
	for _, e := range r.events {
		if e.At.IsZero() || e.Scope != "t" {
			return fmt.Sprintf("bad event %+v", e)
		}
		s := string(e.Kind) + " " + names[e.FileID]
		if e.AttachmentID != "" {
			s += " " + names[e.AttachmentID]
		}
		if e.Owner != (records.Owner{}) {
			s += " " + e.Owner.Type + ":" + e.Owner.ID + "/" + e.Role
		}
		out = append(out, strings.Join(strings.Fields(s), " "))
	}
	return strings.Join(out, "; ")
}

func TestEvents(t *testing.T) {
	ctx := t.Context()
	r := &recorder{}
	e := newEnv(t, nil, records.WithPlugins(r))
	post := records.Owner{Type: "post", ID: "p1"}
	a, b := e.put(t, "t", "a.png"), e.put(t, "t", "b.png")
	aa, _ := e.store.Attach(ctx, "t", a.ID, post, "gallery")
	ab, _ := e.store.Attach(ctx, "t", b.ID, post, "gallery")
	steps := []error{
		e.store.Reorder(ctx, "t", post, "gallery", []string{ab.ID, aa.ID}),
		e.store.Detach(ctx, "t", aa.ID),
		e.store.Detach(ctx, "t", aa.ID), // unknown now: no event
		e.store.Trash(ctx, "t", a.ID),
		e.store.Trash(ctx, "t", a.ID), // already trashed: no event
		e.store.Restore(ctx, "t", a.ID),
		e.store.Restore(ctx, "t", a.ID), // already live: no event
		only(e.store.Replace(ctx, "t", a.ID, strings.NewReader(pngData+"-v2"), "", pngPolicy)),
		e.store.Purge(ctx, "t", b.ID), // its attachment goes without an event
	}
	for i, err := range steps {
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	names := map[string]string{a.ID: "a", b.ID: "b", aa.ID: "att-a", ab.ID: "att-b"}
	want := "file.created a; file.created b; " +
		"attachment.created a att-a post:p1/gallery; attachment.created b att-b post:p1/gallery; " +
		"attachments.reordered post:p1/gallery; attachment.deleted a att-a post:p1/gallery; " +
		"file.trashed a; file.restored a; file.replaced a; file.purged b"
	if got := r.log(names); got != want {
		t.Fatalf("events:\n got  %s\n want %s", got, want)
	}
	if len(records.EventKindValues) != 12 {
		t.Fatalf("EventKindValues lists %d kinds", len(records.EventKindValues))
	}
}

// TestEventRefusalRollsBack proves OnEvent runs inside the change's
// transaction: refusing the event undoes the change.
func TestEventRefusalRollsBack(t *testing.T) {
	ctx := t.Context()
	post := records.Owner{Type: "post", ID: "p1"}
	for _, kind := range records.EventKindValues {
		t.Run(string(kind), func(t *testing.T) {
			r := &recorder{}
			e := newEnv(t, nil, records.WithPlugins(r))
			f := e.put(t, "t", "a.png")
			att, _ := e.store.Attach(ctx, "t", f.ID, post, "gallery")
			r.refuse = kind
			var err error
			check := func() bool { return true }
			switch kind {
			case records.EventFileCreated:
				_, _ = e.disk.WriteBytes(ctx, "b.png", []byte(pngData), filesystem.WriteOptions{})
				_, err = e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "b.png"})
				check = func() bool {
					page, _ := e.store.Files(ctx, records.FileQuery{Scope: "t"})
					return len(page.Files) == 1
				}
			case records.EventFileTrashed:
				err = e.store.Trash(ctx, "t", f.ID)
				check = func() bool { got, _ := e.store.File(ctx, "t", f.ID); return got.DeletedAt == nil }
			case records.EventFileRestored:
				_ = e.store.Trash(ctx, "t", f.ID)
				err = e.store.Restore(ctx, "t", f.ID)
				check = func() bool { got, _ := e.store.File(ctx, "t", f.ID); return got.DeletedAt != nil }
			case records.EventFileReplaced:
				_, err = e.store.Replace(ctx, "t", f.ID, strings.NewReader(pngData+"-v2"), "", upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}})
				check = func() bool {
					got, _ := e.store.File(ctx, "t", f.ID)
					return got.Key == f.Key && countObjects(t, e.disk) == 1
				}
			case records.EventFileVisibilityChanged:
				err = e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic)
				check = func() bool {
					got, _ := e.store.File(ctx, "t", f.ID)
					obj, _ := e.disk.Stat(ctx, f.Key)
					return got.Visibility == filesystem.VisibilityPrivate && obj.Visibility == filesystem.VisibilityPrivate
				}
			case records.EventFileStatusChanged:
				_, err = e.store.SetStatus(ctx, "t", f.ID, records.StatusFailed, "boom")
				check = func() bool {
					got, _ := e.store.File(ctx, "t", f.ID)
					return got.Status == records.StatusReady && got.StatusDetail == ""
				}
			case records.EventFileReleased:
				q := e.quarantined(t, "q.png")
				_, err = e.store.PassScan(ctx, "t", q.ID)
				check = func() bool { got, _ := e.store.File(ctx, "t", q.ID); return got.Quarantined() }
			case records.EventFileMoved:
				me := newMoveEnv(t, nil, records.WithPlugins(r))
				mf := me.record(t, me.fast, "m.png", records.NewFile{})
				_, err = me.store.MoveFile(ctx, "t", mf.ID, "media")
				check = func() bool {
					got, _ := me.store.File(ctx, "t", mf.ID)
					copied, _ := me.media.Exists(ctx, mf.Key)
					kept, _ := me.fast.Exists(ctx, mf.Key)
					return got.Disk == "fast" && !copied && kept
				}
			case records.EventFilePurged:
				err = e.store.Purge(ctx, "t", f.ID)
				check = func() bool { _, err := e.store.File(ctx, "t", f.ID); return err == nil && countObjects(t, e.disk) == 1 }
			case records.EventAttachmentCreated:
				_, err = e.store.Attach(ctx, "t", f.ID, post, "cover")
				check = func() bool { got, _ := e.store.Attachments(ctx, "t", post, "cover"); return len(got) == 0 }
			case records.EventAttachmentDeleted:
				err = e.store.Detach(ctx, "t", att.ID)
				check = func() bool { _, err := e.store.Attachment(ctx, "t", att.ID); return err == nil }
			case records.EventAttachmentsOrdered:
				f2 := e.put(t, "t", "c.png")
				att2, _ := e.store.Attach(ctx, "t", f2.ID, post, "gallery")
				err = e.store.Reorder(ctx, "t", post, "gallery", []string{att2.ID, att.ID})
				check = func() bool {
					got, _ := e.store.Attachments(ctx, "t", post, "gallery")
					return got[0].Attachment.ID == att.ID
				}
			}
			if err == nil || !strings.Contains(err.Error(), "refused") {
				t.Fatalf("err = %v, want the refusal", err)
			}
			if !check() {
				t.Fatal("the refused change was kept")
			}
		})
	}
}

// TestRetrashKeepsTheClock proves trashing a trashed file keeps its
// original trash time, which SweepTrash purges by.
func TestRetrashKeepsTheClock(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f := e.put(t, "t", "a.png")
	_ = e.store.Trash(ctx, "t", f.ID)
	first, _ := e.store.File(ctx, "t", f.ID)
	_ = e.store.Trash(ctx, "t", f.ID)
	again, _ := e.store.File(ctx, "t", f.ID)
	if first.DeletedAt == nil || !again.DeletedAt.Equal(*first.DeletedAt) {
		t.Fatalf("trash time %v moved to %v", first.DeletedAt, again.DeletedAt)
	}
}

func TestEmit(t *testing.T) {
	ctx := t.Context()
	r := &recorder{}
	e := newEnv(t, nil, records.WithPlugins(r, named("folders")))
	inTx := func(fn func(ctx context.Context) error) error {
		return e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error { return fn(ctx) })
	}
	err := inTx(func(ctx context.Context) error {
		return e.store.Emit(ctx, named("folders"), records.Event{Kind: "folders.moved", Scope: "t", Subject: "fld_1", Detail: map[string]string{"parent": "fld_2"}})
	})
	if err != nil || len(r.events) != 1 {
		t.Fatalf("Emit = %v, events %+v", err, r.events)
	}
	if got := r.events[0]; got.Kind != "folders.moved" || got.Subject != "fld_1" || got.Detail["parent"] != "fld_2" || got.At.IsZero() {
		t.Fatalf("event %+v", got)
	}

	// A hook's error comes back, and the caller's transaction rolls back.
	r.refuse = "folders.deleted"
	err = inTx(func(ctx context.Context) error {
		return e.store.Emit(ctx, named("folders"), records.Event{Kind: "folders.deleted", Scope: "t"})
	})
	if err == nil || !strings.Contains(err.Error(), "refused folders.deleted") {
		t.Fatalf("refused Emit = %v", err)
	}

	bad := []struct {
		name string
		from records.Plugin
		kind records.EventKind
	}{
		{"core kind", named("folders"), records.EventFileCreated},
		{"another plugin's kind", named("folders"), "tags.added"},
		{"no action", named("folders"), "folders."},
		{"bad action", named("folders"), "folders.Moved"},
		{"nested action", named("folders"), "folders.a.b"},
		{"too long", named("folders"), records.EventKind("folders." + strings.Repeat("a", records.MaxEventKindBytes))},
		{"not installed", named("tags"), "tags.added"},
		{"nil plugin", nil, "folders.moved"},
	}
	for _, c := range bad {
		err := inTx(func(ctx context.Context) error {
			return e.store.Emit(ctx, c.from, records.Event{Kind: c.kind, Scope: "t"})
		})
		if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: err = %v, want invalid_argument", c.name, err)
		}
	}
	// A plugin named after a core kind's prefix still cannot forge it.
	f := newEnv(t, nil, records.WithPlugins(r, named("file")))
	err = f.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		return f.store.Emit(ctx, named("file"), records.Event{Kind: records.EventFilePurged, Scope: "t"})
	})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	// Outside a transaction.
	err = e.store.Emit(ctx, named("folders"), records.Event{Kind: "folders.moved", Scope: "t"})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if len(r.events) != 1 {
		t.Fatalf("a refused Emit reached the hooks: %+v", r.events)
	}
}
