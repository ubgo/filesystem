package variants_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// twoDiskEnv is an env whose Manager also has a "cdn" disk, built with
// the given plugin options and Store options.
func twoDiskEnv(t *testing.T, opts []variants.Option, storeOpts ...records.Option) (*env, *filesystem.Disk) {
	t.Helper()
	db, _ := recordstest.Open(t)
	media, _ := filesystem.NewDisk("media", memory.New())
	cdn, _ := filesystem.NewDisk("cdn", memory.New())
	mgr, _ := filesystem.NewManager(media, cdn)
	p, err := variants.New(transforms, imaging.Options{}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	s, err := records.New(db, recordstest.Dialect(), mgr, append(storeOpts, records.WithPlugins(p))...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &env{db: db, disk: media, store: s, plugin: p}, cdn
}

// objects lists every key on d.
func objects(t *testing.T, d *filesystem.Disk) []string {
	t.Helper()
	var keys []string
	for e, err := range d.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, e.Key)
	}
	return keys
}

// TestLocation_CustomDiskAndPrefix pins B10: WithLocation puts variants
// on the disk and under the prefix it returns, the plugin still appends
// the generation-unique part, and the row, the purge and SweepOrphans
// follow the variant there.
func TestLocation_CustomDiskAndPrefix(t *testing.T) {
	ctx := t.Context()
	var asked []string
	loc := func(f records.File, name string) (string, string, error) {
		asked = append(asked, name)
		return "cdn", "thumbs/" + f.Scope + "/" + f.ID + "/", nil
	}
	e, cdn := twoDiskEnv(t, []variants.Option{variants.WithLocation(loc)})
	f := e.image(t, "photos/a.png", pngBytes(t, 200, 100), filesystem.VisibilityPrivate)
	v, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	if err != nil {
		t.Fatal(err)
	}
	prefix := "thumbs/t/" + f.ID + "/thumb-" + v.Digest + "-"
	if v.Disk != "cdn" || !strings.HasPrefix(v.Key, prefix) || !strings.HasSuffix(v.Key, ".jpg") || len(v.Key) <= len(prefix)+len(".jpg") {
		t.Fatalf("variant at %s:%s, want cdn:%s<id>.jpg", v.Disk, v.Key, prefix)
	}
	if len(asked) != 1 || asked[0] != "thumb" {
		t.Fatalf("location asked for %v", asked)
	}
	if ok, _ := cdn.Exists(ctx, v.Key); !ok {
		t.Fatal("the variant is not on the cdn disk")
	}
	if keys := objects(t, e.disk); len(keys) != 1 || keys[0] != "photos/a.png" {
		t.Fatalf("media disk holds %v, want only the original", keys)
	}
	if again, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb"); err != nil || again.ID != v.ID {
		t.Fatalf("cached lookup = %+v, %v", again, err)
	}
	if n, err := e.store.SweepOrphans(ctx, "cdn", "", time.Now().Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("sweep of cdn deleted %d, %v", n, err)
	}
	if err := e.store.Purge(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if keys := objects(t, cdn); len(keys) != 0 {
		t.Fatalf("the variant outlived its purged file on cdn: %v", keys)
	}
}

// TestLocation_ErrorFailsClosed pins B10: a Location error writes nothing,
// records no row, fails the request with the error, and an eager
// generation marks the file failed.
func TestLocation_ErrorFailsClosed(t *testing.T) {
	ctx := t.Context()
	errNoRoom := filesystem.Errorf(filesystem.CodeUnavailable, "no room on the cdn")
	loc := func(records.File, string) (string, string, error) { return "", "", errNoRoom }
	var eagerErr error
	onEager := func(_ context.Context, _ records.File, _ string, err error) { eagerErr = err }
	e, cdn := twoDiskEnv(t, []variants.Option{variants.WithLocation(loc), variants.WithEager(onEager, "thumb")})
	f := e.image(t, "photos/a.png", pngBytes(t, 200, 100), filesystem.VisibilityPrivate)
	if !errors.Is(eagerErr, errNoRoom) {
		t.Fatalf("eager error = %v", eagerErr)
	}
	if got, err := e.store.File(ctx, "t", f.ID); err != nil || got.Status != records.StatusFailed {
		t.Fatalf("status = %q %q, %v", got.Status, got.StatusDetail, err)
	}
	if _, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb"); !errors.Is(err, errNoRoom) {
		t.Fatalf("Get = %v", err)
	}
	if keys := objects(t, e.disk); len(keys) != 1 {
		t.Fatalf("media disk holds %v, want only the original", keys)
	}
	if keys := objects(t, cdn); len(keys) != 0 {
		t.Fatalf("cdn disk holds %v", keys)
	}
	var n int
	if err := e.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+variants.TableNameVariants).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d variant rows, %v", n, err)
	}
	// An unknown disk is refused the same way.
	e2, _ := twoDiskEnv(t, []variants.Option{variants.WithLocation(func(records.File, string) (string, string, error) { return "nowhere", "", nil })})
	g := e2.image(t, "photos/b.png", pngBytes(t, 50, 50), filesystem.VisibilityPrivate)
	if _, err := e2.plugin.Get(ctx, e2.store, "t", g.ID, "thumb"); err == nil {
		t.Fatal("a variant was stored on an unknown disk")
	}
	if keys := objects(t, e2.disk); len(keys) != 1 {
		t.Fatalf("media disk holds %v, want only the original", keys)
	}
}

// TestLocation_NilRefused: a nil Location is a configuration mistake.
func TestLocation_NilRefused(t *testing.T) {
	_, err := variants.New(transforms, imaging.Options{}, variants.WithLocation(nil))
	wantCode(t, err, filesystem.CodeInvalidArgument)
}

// TestStoreClock pins I2 for variants: a variant's CreatedAt and the
// expiry check of its file follow the Store's clock (records.WithClock),
// not the wall clock.
func TestStoreClock(t *testing.T) {
	ctx := t.Context()
	fake := time.Date(2020, 3, 4, 5, 6, 7, 0, time.UTC)
	e, _ := twoDiskEnv(t, nil, records.WithClock(func() time.Time { return fake }))
	// Expired by the wall clock, not yet by the Store's.
	expires := fake.Add(time.Hour)
	if _, err := e.disk.WriteBytes(ctx, "a.png", pngBytes(t, 60, 60), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "a.png", ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	// Get refuses an expired file's variants (linkable), as URL does.
	v, err := e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
	if err != nil {
		t.Fatalf("a file live by the Store's clock got no variant: %v", err)
	}
	if !v.CreatedAt.Equal(fake) {
		t.Fatalf("CreatedAt = %v, want the Store's %v", v.CreatedAt, fake)
	}
}
