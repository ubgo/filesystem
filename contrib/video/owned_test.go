package video_test

import (
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video"
	"github.com/ubgo/filesystem/records"
)

// sweepAll runs SweepOrphans over the whole of disk, every object old
// enough, and returns how many it deleted.
func sweepAll(t *testing.T, s *records.Store, disk string) int {
	t.Helper()
	n, err := s.SweepOrphans(t.Context(), disk, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestOwnedObjects_SweepKeepsPosters pins the ObjectOwner: SweepOrphans
// never deletes a poster whose file row (live or trashed) names it, on
// the file's disk or on a WithLocation disk, and still deletes one whose
// file is gone.
func TestOwnedObjects_SweepKeepsPosters(t *testing.T) {
	body := clip(t)
	onCDN := func(f records.File, _ string) (string, string, error) { return "cdn", "posters/" + f.ID + "-", nil }
	for name, opts := range map[string][]video.PluginOption{
		"default":  {video.WithPoster(0)},
		"location": {video.WithPoster(0), video.WithLocation(onCDN)},
	} {
		t.Run(name, func(t *testing.T) {
			e, cdn := twoDiskEnv(t, video.NewPlugin(video.Tool{}, opts...))
			f, err := e.upload(t, body)
			if err != nil {
				t.Fatal(err)
			}
			disk, key, ok := video.PosterLocation(f)
			if !ok {
				t.Fatalf("no poster: %v", f.Metadata)
			}
			on := map[string]*filesystem.Disk{"media": e.disk, "cdn": cdn}[disk]
			if n := sweepAll(t, e.store, disk); n != 0 {
				t.Fatalf("the sweep deleted %d objects of a live video", n)
			}
			if err := e.store.Trash(t.Context(), scope, f.ID); err != nil {
				t.Fatal(err)
			}
			if n := sweepAll(t, e.store, disk); n != 0 || !existsOn(t, on, key) {
				t.Fatalf("the sweep deleted %d objects of a trashed video", n)
			}
			// A poster left behind after its file's purge (its deletion
			// failed) is an orphan.
			if err := e.store.Purge(t.Context(), scope, f.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := on.WriteBytes(t.Context(), key, []byte("left behind"), filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			if n := sweepAll(t, e.store, disk); n != 1 || existsOn(t, on, key) {
				t.Fatalf("the orphaned poster survived the sweep (%d deleted)", n)
			}
		})
	}
}

// TestMoveFileDisk_MovesThePoster pins the FileDiskMover: MoveFile takes a
// poster on the file's disk along, and leaves one WithLocation put on a
// disk of its own.
func TestMoveFileDisk_MovesThePoster(t *testing.T) {
	body := clip(t)
	e, cdn := twoDiskEnv(t, video.NewPlugin(video.Tool{}, video.WithPoster(0)))
	f, err := e.upload(t, body)
	if err != nil {
		t.Fatal(err)
	}
	_, key, _ := video.PosterLocation(f)
	moved, err := e.store.MoveFile(t.Context(), scope, f.ID, "cdn")
	if err != nil {
		t.Fatal(err)
	}
	if disk, k, ok := video.PosterLocation(moved); !ok || disk != "cdn" || k != key {
		t.Fatalf("poster after the move at %s:%s", disk, k)
	}
	if !existsOn(t, cdn, key) || existsOn(t, e.disk, key) {
		t.Fatalf("poster on cdn %v, left on media %v", existsOn(t, cdn, key), existsOn(t, e.disk, key))
	}
	// A poster on a disk of its own stays put.
	elsewhere := func(f records.File, _ string) (string, string, error) { return "cdn", "posters/" + f.ID + "-", nil }
	e2, cdn2 := twoDiskEnv(t, video.NewPlugin(video.Tool{}, video.WithPoster(0), video.WithLocation(elsewhere)))
	g, err := e2.upload(t, body)
	if err != nil {
		t.Fatal(err)
	}
	_, gKey, _ := video.PosterLocation(g)
	movedG, err := e2.store.MoveFile(t.Context(), scope, g.ID, "cdn")
	if err != nil {
		t.Fatal(err)
	}
	if disk, k, _ := video.PosterLocation(movedG); disk != "cdn" || k != gKey || !existsOn(t, cdn2, gKey) {
		t.Fatalf("a located poster moved: %s:%s", disk, k)
	}
}

// existsOn reports whether key is on d.
func existsOn(t *testing.T, d *filesystem.Disk, key string) bool {
	t.Helper()
	ok, err := d.Exists(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}
