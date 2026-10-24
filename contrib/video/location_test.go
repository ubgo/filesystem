package video_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// twoDiskEnv is newEnv with a second disk, "cdn", in the Manager.
func twoDiskEnv(t *testing.T, p *video.Plugin) (*env, *filesystem.Disk) {
	t.Helper()
	media, _ := filesystem.NewDisk("media", memory.New())
	cdn, _ := filesystem.NewDisk("cdn", memory.New())
	mgr, err := filesystem.NewManager(media, cdn)
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
	return &env{disk: media, store: store}, cdn
}

// keysOn lists every key on d.
func keysOn(t *testing.T, d *filesystem.Disk) []string {
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

// TestLocation_CustomDiskAndPrefix pins B10: WithLocation puts the poster
// on the disk and under the prefix it returns, the disk is recorded, and
// a replacement and the purge delete it there.
func TestLocation_CustomDiskAndPrefix(t *testing.T) {
	body := clip(t)
	loc := func(f records.File, name string) (string, string, error) {
		return "cdn", "posters/" + f.Scope + "/" + name + "-", nil
	}
	e, cdn := twoDiskEnv(t, video.NewPlugin(video.Tool{}, video.WithPoster(0), video.WithLocation(loc)))
	f, err := e.upload(t, body)
	if err != nil {
		t.Fatal(err)
	}
	disk, key, ok := video.PosterLocation(f)
	if !ok || disk != "cdn" || f.Metadata[video.MetaPosterDisk] != "cdn" || !strings.HasPrefix(key, "posters/"+scope+"/poster-") || !strings.HasSuffix(key, video.PosterSuffix) {
		t.Fatalf("poster at %s:%s (%v)", disk, key, f.Metadata)
	}
	if ok, _ := cdn.Exists(t.Context(), key); !ok {
		t.Fatal("the poster is not on the cdn disk")
	}
	if keys := keysOn(t, e.disk); len(keys) != 1 || keys[0] != f.Key {
		t.Fatalf("media disk holds %v, want only the video", keys)
	}
	next, err := e.store.Replace(t.Context(), scope, f.ID, bytes.NewReader(body), "", videoPolicy)
	if err != nil {
		t.Fatal(err)
	}
	_, nextKey, _ := video.PosterLocation(next)
	if ok, _ := cdn.Exists(t.Context(), key); ok || nextKey == key {
		t.Fatalf("the old poster outlived the replacement (new %q)", nextKey)
	}
	if err := e.store.Purge(t.Context(), scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if keys := keysOn(t, cdn); len(keys) != 0 {
		t.Fatalf("posters outlived their purged video: %v", keys)
	}
}

// TestLocation_ErrorFailsClosed pins B10: a Location error fails the
// upload, and nothing stays on either disk.
func TestLocation_ErrorFailsClosed(t *testing.T) {
	body := clip(t)
	errNoRoom := errors.New("no room on the cdn")
	loc := func(records.File, string) (string, string, error) { return "", "", errNoRoom }
	e, cdn := twoDiskEnv(t, video.NewPlugin(video.Tool{}, video.WithPoster(0), video.WithLocation(loc)))
	if _, err := e.upload(t, body); !errors.Is(err, errNoRoom) {
		t.Fatalf("upload = %v", err)
	}
	if keys := append(keysOn(t, e.disk), keysOn(t, cdn)...); len(keys) != 0 {
		t.Fatalf("objects outlived the failed upload: %v", keys)
	}
}

// TestLocation_Default: without WithLocation (and with a nil one) the
// poster stays beside its video on the video's disk, and no disk is
// recorded.
func TestLocation_Default(t *testing.T) {
	body := clip(t)
	e, _ := twoDiskEnv(t, video.NewPlugin(video.Tool{}, video.WithPoster(time.Second), video.WithLocation(nil)))
	f, err := e.upload(t, body)
	if err != nil {
		t.Fatal(err)
	}
	disk, key, ok := video.PosterLocation(f)
	if _, recorded := f.Metadata[video.MetaPosterDisk]; !ok || recorded || disk != "media" || !strings.HasPrefix(key, f.Key+video.PosterInfix) {
		t.Fatalf("poster at %s:%s (%v)", disk, key, f.Metadata)
	}
}

// TestLocation_UnknownDisk: a Location naming a disk the Store's manager
// does not have fails the upload as an error of its own would.
func TestLocation_UnknownDisk(t *testing.T) {
	body := clip(t)
	loc := func(records.File, string) (string, string, error) { return "nowhere", "posters/", nil }
	e, cdn := twoDiskEnv(t, video.NewPlugin(video.Tool{}, video.WithPoster(0), video.WithLocation(loc)))
	if _, err := e.upload(t, body); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("upload = %v", err)
	}
	if keys := append(keysOn(t, e.disk), keysOn(t, cdn)...); len(keys) != 0 {
		t.Fatalf("objects outlived the failed upload: %v", keys)
	}
	if _, _, ok := video.PosterLocation(records.File{}); ok {
		t.Fatal("PosterLocation found a poster on a file without one")
	}
}
