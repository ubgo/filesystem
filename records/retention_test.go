package records_test

import (
	"slices"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// TestRetention: attachments past their rule's age are detached, a file
// with nothing left is purged, a file still used elsewhere is kept, and a
// dry run reports exactly what the real run then does without changing
// anything.
func TestRetention(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, records.WithRetention(records.Retention{OwnerType: "post", Role: "chat", MaxAge: time.Hour}))
	post, user := records.Owner{Type: "post", ID: "p1"}, records.Owner{Type: "user", ID: "u1"}
	old := e.put(t, "t", "old.png")
	shared := e.put(t, "t", "shared.png")
	oldAtt, _ := e.store.Attach(ctx, "t", old.ID, post, "chat")
	sharedAtt, _ := e.store.Attach(ctx, "t", shared.ID, post, "chat")
	if _, err := e.store.Attach(ctx, "t", shared.ID, user, "avatar"); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(2 * time.Hour)
	fresh := e.put(t, "t", "fresh.png")
	if _, err := e.store.Attach(ctx, "t", fresh.ID, post, "chat"); err != nil {
		t.Fatal(err)
	}
	other := e.put(t, "t", "cover.png")
	if _, err := e.store.Attach(ctx, "t", other.ID, post, "cover"); err != nil {
		t.Fatal(err)
	}
	now := e.now.Add(30 * time.Minute)

	ids := func(r records.RetentionReport) ([]string, []string) {
		var d []string
		for _, a := range r.Detached {
			d = append(d, a.ID)
		}
		return d, r.Purged
	}
	plan, err := e.store.SweepRetention(ctx, now, true)
	if err != nil {
		t.Fatal(err)
	}
	wantDetached, wantPurged := []string{oldAtt.ID, sharedAtt.ID}, []string{old.ID}
	if d, p := ids(plan); !slices.Equal(d, wantDetached) || !slices.Equal(p, wantPurged) {
		t.Fatalf("dry run: detached %v purged %v", d, p)
	}
	if _, err := e.store.File(ctx, "t", old.ID); err != nil {
		t.Fatalf("the dry run purged: %v", err)
	}
	done, err := e.store.SweepRetention(ctx, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if d, p := ids(done); !slices.Equal(d, wantDetached) || !slices.Equal(p, wantPurged) {
		t.Fatalf("run: detached %v purged %v", d, p)
	}
	if _, err := e.store.File(ctx, "t", old.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatal("the expired file was kept")
	}
	for _, f := range []records.File{shared, fresh, other} {
		if _, err := e.store.File(ctx, "t", f.ID); err != nil {
			t.Fatalf("%s was purged: %v", f.Key, err)
		}
	}
	if again, err := e.store.SweepRetention(ctx, now, false); err != nil || len(again.Detached) != 0 {
		t.Fatalf("a second run: %+v, %v", again, err)
	}
}

// TestRetentionValidation: rules need a registered owner type, a valid
// role and a positive age; without rules the sweep does nothing.
func TestRetentionValidation(t *testing.T) {
	mgr, _ := filesystem.NewManager(newEnv(t, nil).disk)
	for _, r := range []records.Retention{
		{OwnerType: "post", MaxAge: 0},
		{OwnerType: "Bad Type", MaxAge: time.Hour},
		{OwnerType: "post", Role: "Bad Role", MaxAge: time.Hour},
		{OwnerType: "unregistered", MaxAge: time.Hour},
	} {
		if _, err := records.New(openDB(t), testDialect(), mgr, records.WithOwnerTypes("post"), records.WithRetention(r)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%+v: %v", r, err)
		}
	}
	e := newEnv(t, nil)
	if r, err := e.store.SweepRetention(t.Context(), time.Now(), false); err != nil || len(r.Detached)+len(r.Purged) != 0 {
		t.Fatalf("without rules: %+v, %v", r, err)
	}
}
