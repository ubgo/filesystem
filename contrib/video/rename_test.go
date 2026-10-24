package video_test

import (
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// renameEnv is a Store over "media" and "cdn", plus "cdn2", a second name
// for cdn's storage: what a disk is renamed to must already reach its
// objects under the same keys.
func renameEnv(t *testing.T, p *video.Plugin) (*env, *filesystem.Disk) {
	t.Helper()
	media, _ := filesystem.NewDisk("media", memory.New())
	shared := memory.New()
	cdn, _ := filesystem.NewDisk("cdn", shared)
	cdn2, _ := filesystem.NewDisk("cdn2", shared)
	mgr, err := filesystem.NewManager(media, cdn, cdn2)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := recordstest.Open(t)
	store, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &env{disk: media, store: store}, cdn2
}

// TestRenameDisk_RewritesPosterDisk pins the DiskRenamer: after
// Store.RenameDisk, a poster WithLocation put on the renamed disk is
// found under the new name (PosterLocation, the sweep, the purge).
func TestRenameDisk_RewritesPosterDisk(t *testing.T) {
	body := clip(t)
	onCDN := func(f records.File, _ string) (string, string, error) { return "cdn", "posters/" + f.ID + "-", nil }
	e, cdn2 := renameEnv(t, video.NewPlugin(video.Tool{}, video.WithPoster(0), video.WithLocation(onCDN)))
	f, err := e.upload(t, body)
	if err != nil {
		t.Fatal(err)
	}
	_, key, _ := video.PosterLocation(f)
	if _, err := e.store.RenameDisk(t.Context(), "cdn", "cdn2"); err != nil {
		t.Fatal(err)
	}
	got, err := e.store.File(t.Context(), scope, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if disk, k, ok := video.PosterLocation(got); !ok || disk != "cdn2" || k != key || !existsOn(t, cdn2, k) {
		t.Fatalf("poster after the rename at %s:%s (%v)", disk, k, got.Metadata)
	}
	if n := sweepAll(t, e.store, "cdn2"); n != 0 {
		t.Fatalf("the sweep of the renamed disk deleted %d objects", n)
	}
	if err := e.store.Purge(t.Context(), scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if existsOn(t, cdn2, key) {
		t.Fatal("the poster outlived its purged video")
	}
}
