package records_test

import (
	"context"
	"encoding/base64"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

type appUserKey struct{}

// TestWithActorFunc (I3): an application's own actor lookup becomes
// File.UploadedBy; when it names nobody the context's WithActor is still
// read (the share plugin sets one for depositors); without the option
// ActorFrom rules as before; nil is refused.
func TestWithActorFunc(t *testing.T) {
	fromApp := func(ctx context.Context) string {
		id, _ := ctx.Value(appUserKey{}).(string)
		if id == "" {
			return ""
		}
		return "user:" + id
	}
	e := newEnv(t, nil, records.WithActorFunc(fromApp))
	ctx := context.WithValue(t.Context(), appUserKey{}, "42")
	if got := e.store.Actor(ctx); got != "user:42" {
		t.Fatalf("Actor = %q", got)
	}
	f, err := e.store.Upload(ctx, "t", "media", strings.NewReader(pngData), "a.png", images)
	if err != nil {
		t.Fatal(err)
	}
	if f.UploadedBy != "user:42" {
		t.Fatalf("UploadedBy = %q, want the custom actor", f.UploadedBy)
	}
	fallback := records.WithActor(t.Context(), "share:shr_1")
	if got := e.store.Actor(fallback); got != "share:shr_1" {
		t.Fatalf("Actor with an empty custom answer = %q, want WithActor's", got)
	}
	both := records.WithActor(ctx, "ignored")
	if got := e.store.Actor(both); got != "user:42" {
		t.Fatalf("Actor = %q: the custom lookup must win when it names someone", got)
	}

	d := newEnv(t, nil)
	if got := d.store.Actor(records.WithActor(ctx, "alice")); got != "alice" {
		t.Fatalf("default Actor = %q, want ActorFrom's", got)
	}
	if _, err := records.New(e.db, testDialect(), e.store.Disks(), records.WithActorFunc(nil)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("nil actor func: %v", err)
	}
}

// TestCustomServeKinds (I14): a plugin's or an application's own read kind
// "access.<name>.<action>" reaches the ServeHooks; anything else outside
// the built-in kinds is refused before any hook, as before.
func TestCustomServeKinds(t *testing.T) {
	log := &serveLog{name: "log"}
	e := newEnv(t, nil, records.WithPlugins(log))
	f := e.put(t, "t", "a.png")
	if err := e.store.Served(t.Context(), records.Served{Kind: "access.zip.download", Scope: "t", FileID: f.ID, Bytes: 3}); err != nil {
		t.Fatalf("custom kind refused: %v", err)
	}
	if len(log.got) != 1 || log.got[0].Kind != "access.zip.download" {
		t.Fatalf("hooks saw %+v", log.got)
	}
	for _, k := range []records.ServeKind{"zip.download", "access.zip", "access.Zip.download", "access.zip.download.extra", "access." + records.ServeKind(strings.Repeat("a", 60)) + ".b", "file.created"} {
		if err := e.store.Served(t.Context(), records.Served{Kind: k, Scope: "t", FileID: f.ID}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("kind %q: %v, want invalid_argument", k, err)
		}
	}
	for _, k := range records.ServeKindValues {
		if !k.Valid() {
			t.Errorf("built-in kind %q not valid", k)
		}
	}
	if len(log.got) != 1 {
		t.Fatalf("a refused kind reached the hooks: %+v", log.got)
	}
}

// TestFileOrders (I20): each order sorts by its key then ID, pages through
// every file exactly once with keyset cursors, refuses a cursor of another
// order, and the default stays newest first with its old cursor format.
func TestFileOrders(t *testing.T) {
	e := newEnv(t, nil)
	sizes := map[string]int{"b.png": 30, "a.png": 10, "c.png": 20, "a2.png": 10}
	var made []records.File
	for _, name := range []string{"b.png", "a.png", "c.png", "a2.png"} {
		if _, err := e.disk.WriteBytes(t.Context(), name, []byte(strings.Repeat("x", sizes[name])), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		f, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: "t", Disk: "media", Key: name, Filename: name, Size: int64(sizes[name])})
		if err != nil {
			t.Fatal(err)
		}
		made = append(made, f)
	}
	names := func(fs []records.File) []string {
		var out []string
		for _, f := range fs {
			out = append(out, f.Filename)
		}
		return out
	}
	// a.png and a2.png have one size: that order breaks the tie by ID.
	lo, hi := "a.png", "a2.png"
	if made[3].ID < made[1].ID {
		lo, hi = hi, lo
	}
	want := map[records.FileOrder][]string{
		records.OrderNewest:   {"a2.png", "c.png", "a.png", "b.png"},
		records.OrderOldest:   {"b.png", "a.png", "c.png", "a2.png"},
		records.OrderName:     {"a.png", "a2.png", "b.png", "c.png"},
		records.OrderNameDesc: {"c.png", "b.png", "a2.png", "a.png"},
		records.OrderLargest:  {"b.png", "c.png", hi, lo},
		records.OrderSmallest: {lo, hi, "c.png", "b.png"},
	}
	for _, o := range records.FileOrderValues {
		all, err := e.store.Files(t.Context(), records.FileQuery{Scope: "t", Order: o})
		if err != nil {
			t.Fatal(err)
		}
		if got := names(all.Files); !slices.Equal(got, want[o]) {
			t.Errorf("order %q: %v, want %v", o, got, want[o])
		}
		var paged []records.File
		cursor := ""
		for range 10 {
			page, err := e.store.Files(t.Context(), records.FileQuery{Scope: "t", Order: o, Limit: 1, Cursor: cursor})
			if err != nil {
				t.Fatalf("order %q page: %v", o, err)
			}
			paged = append(paged, page.Files...)
			if cursor = page.NextCursor; cursor == "" {
				break
			}
		}
		if got := names(paged); !slices.Equal(got, want[o]) {
			t.Errorf("order %q paged: %v, want %v", o, got, want[o])
		}
	}
	page, _ := e.store.Files(t.Context(), records.FileQuery{Scope: "t", Order: records.OrderName, Limit: 1})
	for _, other := range []records.FileOrder{records.OrderNameDesc, records.OrderLargest, records.OrderNewest} {
		if _, err := e.store.Files(t.Context(), records.FileQuery{Scope: "t", Order: other, Cursor: page.NextCursor}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("a name cursor under order %q: %v", other, err)
		}
	}
	if _, err := e.store.Files(t.Context(), records.FileQuery{Scope: "t", Order: "random"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("an unknown order: %v", err)
	}
	// A cursor that is not base64, and one whose size is not a number.
	forged := base64.RawURLEncoding.EncodeToString([]byte(string(records.OrderLargest) + "|big|fil_1"))
	for _, c := range []string{"!!not base64!!", forged} {
		if _, err := e.store.Files(t.Context(), records.FileQuery{Scope: "t", Order: records.OrderLargest, Cursor: c}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("cursor %q: %v", c, err)
		}
	}
	newest, _ := e.store.Files(t.Context(), records.FileQuery{Scope: "t", Limit: 1})
	last := newest.Files[0]
	if newest.NextCursor != records.EncodeCursor(last.CreatedAt, last.ID) {
		t.Fatal("the default order's cursor format changed")
	}
}

// TestFileOrderNameWithSeparator: a filename holding the cursor separator
// still pages correctly by name.
func TestFileOrderNameWithSeparator(t *testing.T) {
	e := newEnv(t, nil)
	for _, name := range []string{"x|1.png", "x|2.png", "x|3.png"} {
		if _, err := e.disk.WriteBytes(t.Context(), strings.ReplaceAll(name, "|", "_"), []byte(pngData), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: "t", Disk: "media", Key: strings.ReplaceAll(name, "|", "_"), Filename: name}); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	cursor := ""
	for range 5 {
		page, err := e.store.Files(t.Context(), records.FileQuery{Scope: "t", Order: records.OrderName, Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range page.Files {
			got = append(got, f.Filename)
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if !slices.Equal(got, []string{"x|1.png", "x|2.png", "x|3.png"}) {
		t.Fatalf("paged by name: %v", got)
	}
}

// TestSweepRetentionIn (I22): rules given for one scope apply to that
// scope only (a free tenant's 30 days do not touch a pro tenant), a dry
// run changes nothing, and a bad or unregistered rule sweeps nothing.
func TestSweepRetentionIn(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	post := records.Owner{Type: "post", ID: "p1"}
	free := e.put(t, "free", "f.png")
	pro := e.put(t, "pro", "p.png")
	freeAtt, _ := e.store.Attach(ctx, "free", free.ID, post, "chat")
	if _, err := e.store.Attach(ctx, "pro", pro.ID, post, "chat"); err != nil {
		t.Fatal(err)
	}
	now := e.now.Add(48 * time.Hour)
	rules := []records.Retention{{OwnerType: "post", MaxAge: 24 * time.Hour}}

	plan, err := e.store.SweepRetentionIn(ctx, "free", rules, now, true)
	if err != nil || len(plan.Detached) != 1 || plan.Detached[0].ID != freeAtt.ID {
		t.Fatalf("dry run: %+v %v", plan, err)
	}
	if _, err := e.store.File(ctx, "free", free.ID); err != nil {
		t.Fatalf("the dry run purged: %v", err)
	}
	rep, err := e.store.SweepRetentionIn(ctx, "free", rules, now, false)
	if err != nil || len(rep.Detached) != 1 || !slices.Equal(rep.Purged, []string{free.ID}) {
		t.Fatalf("sweep: %+v %v", rep, err)
	}
	if _, err := e.store.File(ctx, "pro", pro.ID); err != nil {
		t.Fatalf("the free tenant's rule reached the pro tenant: %v", err)
	}
	if rep, err := e.store.SweepRetention(ctx, now, false); err != nil || len(rep.Detached) != 0 {
		t.Fatalf("SweepRetention without WithRetention must still do nothing: %+v %v", rep, err)
	}
	for name, bad := range map[string]records.Retention{
		"unregistered": {OwnerType: "order", MaxAge: time.Hour},
		"no age":       {OwnerType: "post"},
		"bad role":     {OwnerType: "post", Role: "Bad Role", MaxAge: time.Hour},
	} {
		if _, err := e.store.SweepRetentionIn(ctx, "pro", []records.Retention{bad}, now, false); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := e.store.File(ctx, "pro", pro.ID); err != nil {
		t.Fatalf("a refused sweep changed something: %v", err)
	}
}

// TestForeignOwnerTypes (C4): types spelled as other web frameworks store them
// attach and list; they stay refused by WithOwnerTypes, and the wider rule
// still refuses what would break a path segment or the column.
func TestForeignOwnerTypes(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, records.WithForeignOwnerTypes("User", "Admin::User", `App\Models\Post`))
	f := e.put(t, "t", "a.png")
	for _, typ := range []string{"User", "Admin::User", `App\Models\Post`} {
		owner := records.Owner{Type: typ, ID: "1"}
		if _, err := e.store.Attach(ctx, "t", f.ID, owner, "photos"); err != nil {
			t.Fatalf("attach to %q: %v", typ, err)
		}
		list, err := e.store.Attachments(ctx, "t", owner, "photos")
		if err != nil || len(list) != 1 {
			t.Fatalf("list of %q: %v %v", typ, list, err)
		}
	}
	if _, err := e.store.Attach(ctx, "t", f.ID, records.Owner{Type: "Order", ID: "1"}, "photos"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("an unregistered type: %v", err)
	}
	if _, err := records.New(e.db, testDialect(), e.store.Disks(), records.WithOwnerTypes("User")); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("WithOwnerTypes must keep its lowercase rule: %v", err)
	}
	for _, bad := range []string{"", "1User", "App/Models/Post", "Post Model", "a'b", strings.Repeat("A", 65)} {
		if _, err := records.New(e.db, testDialect(), e.store.Disks(), records.WithForeignOwnerTypes(bad)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("foreign type %q: %v", bad, err)
		}
	}
	if _, err := records.New(e.db, testDialect(), e.store.Disks(), records.WithForeignOwnerTypes("User"),
		records.WithRetention(records.Retention{OwnerType: "User", MaxAge: time.Hour})); err != nil {
		t.Fatalf("retention on a foreign type: %v", err)
	}
}
