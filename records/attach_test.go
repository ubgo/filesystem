package records_test

import (
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
)

func TestAttachments(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	post := records.Owner{Type: "post", ID: "p1"}
	a := e.put(t, "t", "1.png")
	b := e.put(t, "t", "2.png")
	c := e.put(t, "t", "3.png")
	var atts []records.Attachment
	for _, f := range []records.File{a, b, c} {
		att, err := e.store.Attach(ctx, "t", f.ID, post, "gallery")
		if err != nil {
			t.Fatal(err)
		}
		atts = append(atts, att)
	}
	if atts[0].Position != 1 || atts[2].Position != 3 || !strings.HasPrefix(atts[0].ID, records.IDPrefixAttachment) {
		t.Errorf("attachments = %+v", atts)
	}
	keys := func() string {
		list, err := e.store.Attachments(ctx, "t", post, "gallery")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range list {
			out = append(out, x.File.Key)
		}
		return strings.Join(out, ",")
	}
	if got := keys(); got != "1.png,2.png,3.png" {
		t.Errorf("order = %s", got)
	}
	if err := e.store.Reorder(ctx, "t", post, "gallery", []string{atts[2].ID, atts[0].ID, atts[1].ID}); err != nil {
		t.Fatal(err)
	}
	if got := keys(); got != "3.png,1.png,2.png" {
		t.Errorf("after reorder = %s", got)
	}
	for _, ids := range [][]string{{atts[0].ID}, {atts[0].ID, atts[1].ID, atts[1].ID}, {atts[0].ID, atts[1].ID, "att_x"}} {
		err := e.store.Reorder(ctx, "t", post, "gallery", ids)
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
	got, err := e.store.Attachment(ctx, "t", atts[1].ID)
	if err != nil || got.FileID != b.ID || got.Owner != post || got.Role != "gallery" {
		t.Errorf("Attachment = %+v, %v", got, err)
	}
	_, err = e.store.Attachment(ctx, "other", atts[1].ID)
	wantCode(t, err, filesystem.CodeNotFound)
	if err := e.store.Detach(ctx, "t", atts[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Detach(ctx, "t", "att_unknown"); err != nil {
		t.Errorf("detach unknown = %v", err)
	}
	if got := keys(); got != "3.png,1.png" {
		t.Errorf("after detach = %s", got)
	}

	_, err = e.store.Attach(ctx, "t", a.ID, post, "gallery")
	wantCode(t, err, filesystem.CodeExists)
	_, err = e.store.Attach(ctx, "t", "fil_missing", post, "gallery")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = e.store.Attach(ctx, "other-scope", a.ID, post, "gallery")
	wantCode(t, err, filesystem.CodeNotFound)
	_ = e.store.Trash(ctx, "t", b.ID)
	_, err = e.store.Attach(ctx, "t", b.ID, post, "cover")
	wantCode(t, err, filesystem.CodeNotFound)

	invalid := []struct {
		owner records.Owner
		role  string
	}{
		{records.Owner{Type: "comment", ID: "1"}, "x"},
		{records.Owner{Type: "post", ID: ""}, "x"},
		{records.Owner{Type: "post", ID: strings.Repeat("x", 192)}, "x"},
		{post, "Bad Role"},
	}
	for _, in := range invalid {
		_, err := e.store.Attach(ctx, "t", a.ID, in.owner, in.role)
		wantCode(t, err, filesystem.CodeInvalidArgument)
		_, err = e.store.Attachments(ctx, "t", in.owner, in.role)
		wantCode(t, err, filesystem.CodeInvalidArgument)
		wantCode(t, e.store.Reorder(ctx, "t", in.owner, in.role, nil), filesystem.CodeInvalidArgument)
	}
}

// TestReorderWithTrashedAttachments: Reorder takes the list clients see
// (Attachments, which leaves trashed files out); the trashed attachments
// keep their relative order after it, so a restored file comes back last.
func TestReorderWithTrashedAttachments(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	post := records.Owner{Type: "post", ID: "p"}
	var atts []records.Attachment
	var files []records.File
	for _, k := range []string{"1.png", "2.png", "3.png", "4.png"} {
		f := e.put(t, "t", k)
		att, err := e.store.Attach(ctx, "t", f.ID, post, "gallery")
		if err != nil {
			t.Fatal(err)
		}
		atts, files = append(atts, att), append(files, f)
	}
	_ = e.store.Trash(ctx, "t", files[2].ID)
	_ = e.store.Trash(ctx, "t", files[0].ID)
	if err := e.store.Reorder(ctx, "t", post, "gallery", []string{atts[3].ID, atts[1].ID}); err != nil {
		t.Fatalf("reorder of the visible list = %v", err)
	}
	wantCode(t, e.store.Reorder(ctx, "t", post, "gallery", []string{atts[3].ID, atts[1].ID, atts[0].ID}), filesystem.CodeInvalidArgument)
	_ = e.store.Restore(ctx, "t", files[2].ID)
	_ = e.store.Restore(ctx, "t", files[0].ID)
	list, _ := e.store.Attachments(ctx, "t", post, "gallery")
	var got []string
	for _, x := range list {
		got = append(got, x.File.Key)
	}
	if strings.Join(got, ",") != "4.png,2.png,1.png,3.png" {
		t.Fatalf("order after restoring = %v", got)
	}
}

func TestRoleLimit(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, records.WithRoleLimit("gallery", 2))
	post := records.Owner{Type: "post", ID: "p1"}
	a, b, c := e.put(t, "t", "a.png"), e.put(t, "t", "b.png"), e.put(t, "t", "c.png")
	attA, err := e.store.Attach(ctx, "t", a.ID, post, "gallery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Attach(ctx, "t", b.ID, post, "gallery"); err != nil {
		t.Fatal(err)
	}
	_, err = e.store.Attach(ctx, "t", c.ID, post, "gallery")
	wantCode(t, err, filesystem.CodeQuotaExceeded)
	if !strings.Contains(err.Error(), "at most 2") {
		t.Fatalf("message = %v", err)
	}
	// The same file twice is still exists, not quota.
	_, err = e.store.Attach(ctx, "t", a.ID, post, "gallery")
	wantCode(t, err, filesystem.CodeExists)
	// A trashed file keeps its place; detaching frees one.
	_ = e.store.Trash(ctx, "t", b.ID)
	_, err = e.store.Attach(ctx, "t", c.ID, post, "gallery")
	wantCode(t, err, filesystem.CodeQuotaExceeded)
	if err := e.store.Detach(ctx, "t", attA.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Attach(ctx, "t", c.ID, post, "gallery"); err != nil {
		t.Fatalf("after a detach: %v", err)
	}
	// Other owners and roles are not limited by it.
	if _, err := e.store.Attach(ctx, "t", a.ID, records.Owner{Type: "post", ID: "p2"}, "gallery"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Attach(ctx, "t", a.ID, post, "cover"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]records.Option{
		{records.WithRoleLimit("gallery", 1)},
		{records.WithRoleLimit("gallery", records.MaxRoleLimit+1)},
		{records.WithRoleLimit("Bad", 3)},
		{records.WithRoleLimit("avatar", 3), records.WithSingleRoles("avatar")},
	} {
		_, err := records.New(e.db, testDialect(), mustManager(t), bad...)
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
}

// TestRoleLimitRace stages a second request taking the last slot between
// an Attach's read of the slots and its insert: the insert collides on the
// derived ID and the caller is told to retry, so the role never passes its
// cap. (SQLite only: the race is staged with a trigger.)
func TestRoleLimitRace(t *testing.T) {
	onlySQLite(t)
	ctx := t.Context()
	e := newEnv(t, nil, records.WithRoleLimit("gallery", 2))
	post := records.Owner{Type: "post", ID: "p1"}
	a, b := e.put(t, "t", "a.png"), e.put(t, "t", "b.png")
	if _, err := e.store.Attach(ctx, "t", a.ID, post, "gallery"); err != nil {
		t.Fatal(err)
	}
	// A trigger takes the slot this Attach chose just before its insert,
	// as a racing request committing between the read and the insert
	// would.
	cols := strings.Join([]string{records.ColAttachmentID, records.ColAttachmentScope, records.ColAttachmentFileID, records.ColAttachmentOwnerType,
		records.ColAttachmentOwnerID, records.ColAttachmentRole, records.ColAttachmentPosition, records.ColAttachmentCreatedAt}, ", ")
	exec(t, e.db, "CREATE TRIGGER take BEFORE INSERT ON "+records.TableNameAttachments+" WHEN NEW."+records.ColAttachmentFileID+" = '"+b.ID+"'"+
		" BEGIN INSERT INTO "+records.TableNameAttachments+" ("+cols+") VALUES (NEW."+records.ColAttachmentID+", NEW."+records.ColAttachmentScope+
		", 'fil_other', NEW."+records.ColAttachmentOwnerType+", NEW."+records.ColAttachmentOwnerID+", NEW."+records.ColAttachmentRole+", 99, NEW."+records.ColAttachmentCreatedAt+"); END")
	_, err := e.store.Attach(ctx, "t", b.ID, post, "gallery")
	wantCode(t, err, filesystem.CodeUnavailable)
	if list, _ := e.store.Attachments(ctx, "t", post, "gallery"); len(list) > 2 {
		t.Fatalf("the role holds %d files", len(list))
	}
}

func mustManager(t *testing.T) *filesystem.Manager {
	t.Helper()
	disk, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := filesystem.NewManager(disk)
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}
