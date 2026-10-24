package fsgorm_test

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3/gormlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"github.com/ubgo/filesystem"
	fsgorm "github.com/ubgo/filesystem/contrib/gorm"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/upload"
)

// env is a Store on the engine under test and GORM over the same
// connection pool.
type env struct {
	store *records.Store
	disk  *filesystem.Disk
	db    *sql.DB
	gorm  *gorm.DB
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, nil)
}

// tablePrefix is the prefix of the prefixed Store in the naming tests.
const tablePrefix = "app_"

// prefixed is what records.WithTablePrefix(tablePrefix) does to a name.
func prefixed(name string) string { return tablePrefix + name }

// newEnvWith is newEnv with the Store's table names mapped by names (nil
// keeps the defaults) and GORM opened with fsgorm.NewNamer(nil, names).
func newEnvWith(t *testing.T, names records.TableNamer) *env {
	t.Helper()
	db, dialect := recordstest.Open(t)
	disk, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := filesystem.NewManager(disk)
	if err != nil {
		t.Fatal(err)
	}
	opts := []records.Option{records.WithOwnerTypes("post")}
	cfg := &gorm.Config{Logger: logger.Discard}
	if names != nil {
		opts = append(opts, records.WithTableNames(names))
		cfg.NamingStrategy = fsgorm.NewNamer(nil, names)
	}
	s, err := records.New(db, dialect, mgr, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	var d gorm.Dialector
	switch recordstest.Engine() {
	case recordstest.Postgres:
		d = postgres.New(postgres.Config{Conn: db})
	case recordstest.MySQL:
		d = mysql.New(mysql.Config{Conn: db, SkipInitializeWithVersion: true})
	default:
		d = gormlite.OpenDB(db)
	}
	g, err := gorm.Open(d, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &env{store: s, disk: disk, db: db, gorm: g}
}

// TestModelsMatchTables: every model field is a column of the Store's
// table, and every column of the table is a model field.
func TestModelsMatchTables(t *testing.T) {
	modelsMatchTables(t, newEnv(t), "")
}

// TestNamerModelsMatchTables: with fsgorm.NewNamer and the Store's
// mapping, the models name the prefixed tables the Store created, and
// their columns match.
func TestNamerModelsMatchTables(t *testing.T) {
	modelsMatchTables(t, newEnvWith(t, prefixed), tablePrefix)
}

func modelsMatchTables(t *testing.T, e *env, prefix string) {
	t.Helper()
	for _, m := range []any{&fsgorm.File{}, &fsgorm.Attachment{}} {
		st := &gorm.Statement{DB: e.gorm}
		if err := st.Parse(m); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(st.Schema.Table, prefix+"fs_") {
			t.Errorf("model names table %q, want the prefix %q", st.Schema.Table, prefix)
		}
		fields := slices.Clone(st.Schema.DBNames)
		slices.Sort(fields)
		types, err := e.gorm.Migrator().ColumnTypes(m)
		if err != nil {
			t.Fatal(err)
		}
		var cols []string
		for _, c := range types {
			cols = append(cols, c.Name())
		}
		slices.Sort(cols)
		if !slices.Equal(fields, cols) {
			t.Errorf("%s: model fields %v, table columns %v", st.Schema.Table, fields, cols)
		}
		if len(cols) == 0 {
			t.Fatalf("%s has no columns: the comparison proves nothing", st.Schema.Table)
		}
	}
}

// TestReadsWhatTheStoreWrote lists, preloads and converts rows the Store
// wrote, and compares them with the Store's own answers.
func TestReadsWhatTheStoreWrote(t *testing.T) {
	readsWhatTheStoreWrote(t, newEnv(t))
}

// TestNamerReadsWhatTheStoreWrote is the same over a Store built with
// WithTableNames and GORM opened with fsgorm.NewNamer: listings, both
// preloads and WithLiveFile's EXISTS all name the prefixed tables.
func TestNamerReadsWhatTheStoreWrote(t *testing.T) {
	readsWhatTheStoreWrote(t, newEnvWith(t, prefixed))
}

// TestNamerDefaults: a zero Namer, a Namer without a mapping and any other
// naming strategy keep the default records names, and a Namer delegates
// the application's own tables to its base strategy.
func TestNamerDefaults(t *testing.T) {
	for _, n := range []schema.Namer{fsgorm.Namer{}, fsgorm.NewNamer(nil, nil), schema.NamingStrategy{TablePrefix: "x_"}} {
		if got := (fsgorm.File{}).TableName(n); got != records.TableNameFiles {
			t.Errorf("%T: files table %q", n, got)
		}
		if got := (fsgorm.Attachment{}).TableName(n); got != records.TableNameAttachments {
			t.Errorf("%T: attachments table %q", n, got)
		}
	}
	n := fsgorm.NewNamer(schema.NamingStrategy{TablePrefix: "x_"}, prefixed)
	if got := n.TableName("Post"); got != "x_posts" {
		t.Errorf("own table %q, want the base strategy's x_posts", got)
	}
	if got := (fsgorm.File{}).TableName(n); got != prefixed(records.TableNameFiles) {
		t.Errorf("files table %q: the base prefix must not apply to records tables", got)
	}
}

// TestNamerDelegates: every schema.Namer method of a Namer answers as its
// base strategy does, and a zero Namer as GORM's default strategy.
func TestNamerDelegates(t *testing.T) {
	base := schema.NamingStrategy{TablePrefix: "x_", SingularTable: true}
	rel := schema.Relationship{Name: "Owner", Schema: &schema.Schema{Table: "posts"}, FieldSchema: &schema.Schema{Table: "users"}}
	for _, c := range []struct {
		n    schema.Namer
		want schema.Namer
	}{{fsgorm.NewNamer(base, prefixed), base}, {fsgorm.Namer{}, schema.NamingStrategy{}}} {
		got := []string{c.n.TableName("Post"), c.n.SchemaName("posts"), c.n.ColumnName("posts", "OwnerID"), c.n.JoinTableName("PostTags"),
			c.n.RelationshipFKName(rel), c.n.CheckerName("posts", "size"), c.n.IndexName("posts", "scope"), c.n.UniqueName("posts", "slug")}
		want := []string{c.want.TableName("Post"), c.want.SchemaName("posts"), c.want.ColumnName("posts", "OwnerID"), c.want.JoinTableName("PostTags"),
			c.want.RelationshipFKName(rel), c.want.CheckerName("posts", "size"), c.want.IndexName("posts", "scope"), c.want.UniqueName("posts", "slug")}
		if !slices.Equal(got, want) {
			t.Errorf("%T: %v, want the base strategy's %v", c.n, got, want)
		}
	}
}

func readsWhatTheStoreWrote(t *testing.T, e *env) {
	t.Helper()
	ctx := t.Context()
	policy := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}}
	var ids []string
	for _, body := range []string{"one", "two", "three"} {
		f, err := e.store.Upload(records.WithActor(ctx, "user:alice"), "t", "media", strings.NewReader(body), body+".txt", policy)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, f.ID)
	}
	if _, err := e.store.Upload(ctx, "other", "media", strings.NewReader("x"), "x.txt", policy); err != nil {
		t.Fatal(err)
	}
	post := records.Owner{Type: "post", ID: "p1"}
	for _, id := range []string{ids[2], ids[0], ids[1]} { // ids[1] is trashed below
		if _, err := e.store.Attach(ctx, "t", id, post, "gallery"); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.store.Trash(ctx, "t", ids[1]); err != nil {
		t.Fatal(err)
	}

	var files []fsgorm.File
	if err := e.gorm.Scopes(fsgorm.InScope("t"), fsgorm.Live, fsgorm.NewestFirst).Preload("Attachments").Find(&files).Error; err != nil {
		t.Fatal(err)
	}
	page, err := e.store.Files(ctx, records.FileQuery{Scope: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(page.Files) {
		t.Fatalf("GORM listed %d files, the Store %d", len(files), len(page.Files))
	}
	for i, f := range files {
		got, err := f.Record()
		if err != nil {
			t.Fatal(err)
		}
		want := page.Files[i]
		if got.ID != want.ID || got.Key != want.Key || got.Size != want.Size || got.SHA256 != want.SHA256 || !got.CreatedAt.Equal(want.CreatedAt) ||
			got.UploadedBy != want.UploadedBy || got.Status != want.Status || got.Visibility != want.Visibility || got.DeletedAt != nil {
			t.Errorf("row %d: GORM %+v, Store %+v", i, got, want)
		}
		if len(f.Attachments) != 1 || f.Attachments[0].OwnerID != post.ID {
			t.Errorf("file %s preloaded %+v", f.ID, f.Attachments)
		}
	}

	var attached []fsgorm.Attachment
	err = e.gorm.Scopes(fsgorm.InScope("t"), fsgorm.WithLiveFile, fsgorm.ByPosition).
		Where("owner_type = ? AND owner_id = ? AND role = ?", post.Type, post.ID, "gallery").Preload("File").Find(&attached).Error
	if err != nil {
		t.Fatal(err)
	}
	store, err := e.store.Attachments(ctx, "t", post, "gallery")
	if err != nil {
		t.Fatal(err)
	}
	if len(attached) != len(store) {
		t.Fatalf("GORM %d attachments, Store %d", len(attached), len(store))
	}
	for i, a := range attached {
		if a.ID != store[i].Attachment.ID || a.File == nil || a.File.ID != store[i].File.ID || a.Position != int64(store[i].Attachment.Position) {
			t.Errorf("attachment %d: GORM %+v, Store %+v", i, a, store[i])
		}
	}

	var trashed fsgorm.File
	if err := e.gorm.Scopes(fsgorm.InScope("t")).First(&trashed, "id = ?", ids[1]).Error; err != nil {
		t.Fatal(err)
	}
	if r, err := trashed.Record(); err != nil || r.DeletedAt == nil {
		t.Fatalf("a trashed file read back as %+v, %v", r, err)
	}
}

// TestRecordMetadata: metadata decodes to the map the Store stored, and a
// row whose metadata is not a JSON object is refused, not emptied.
func TestRecordMetadata(t *testing.T) {
	f := fsgorm.File{ID: "f1", Metadata: `{"alt":"a cat"}`}
	r, err := f.Record()
	if err != nil || r.Metadata["alt"] != "a cat" {
		t.Fatalf("Record = %+v, %v", r.Metadata, err)
	}
	if r, err := (fsgorm.File{ID: "f2", Metadata: "{}"}).Record(); err != nil || r.Metadata != nil {
		t.Fatalf("empty metadata = %+v, %v", r.Metadata, err)
	}
	if _, err := (fsgorm.File{ID: "f3", Metadata: "not json"}).Record(); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("malformed metadata = %v", err)
	}
}

// TestRecordSourceAndExpiry: the source columns become File.Source, the
// expiry and trash times come back in UTC (nil stays nil), and any
// non-zero view_once state (unviewed or viewed) is a view-once file.
func TestRecordSourceAndExpiry(t *testing.T) {
	at := time.Date(2026, 10, 3, 14, 0, 0, 0, time.FixedZone("x", 2*3600))
	r, err := fsgorm.File{ID: "f1", SourceChannel: "api", SourceClient: "ios/1.2", SourceIP: "203.0.113.7", ExpiresAt: &at, DeletedAt: &at, ViewOnce: 2}.Record()
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != (records.Source{Channel: "api", Client: "ios/1.2", IP: "203.0.113.7"}) || !r.ViewOnce {
		t.Errorf("Record = %+v", r)
	}
	for name, got := range map[string]*time.Time{"ExpiresAt": r.ExpiresAt, "DeletedAt": r.DeletedAt} {
		if got == nil || !got.Equal(at) || got.Location() != time.UTC {
			t.Errorf("%s = %v, want %v in UTC", name, got, at)
		}
	}
	if r, err := (fsgorm.File{ID: "f2"}).Record(); err != nil || r.ExpiresAt != nil || r.DeletedAt != nil || r.ViewOnce {
		t.Errorf("a plain row = %+v, %v", r, err)
	}
}

// TestRecordChecksums: the checksums column decodes to the map the Store
// stored, NULL to nil, and a value that is not a JSON object is refused.
func TestRecordChecksums(t *testing.T) {
	sums := `{"md5":"d41d8cd98f00b204e9800998ecf8427e"}`
	r, err := (fsgorm.File{ID: "f1", Checksums: &sums}).Record()
	if err != nil || r.Checksums["md5"] != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatalf("Record = %+v, %v", r.Checksums, err)
	}
	if r, err := (fsgorm.File{ID: "f2"}).Record(); err != nil || r.Checksums != nil {
		t.Fatalf("NULL checksums = %+v, %v", r.Checksums, err)
	}
	bad := "not json"
	if _, err := (fsgorm.File{ID: "f3", Checksums: &bad}).Record(); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("malformed checksums = %v", err)
	}
}
