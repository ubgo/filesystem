// Command labels uploads images to an opted-in scope and prints the tags
// contrib/vision wrote from Google Cloud Vision's labels. Each image is
// billed by Google (the first 1000 a month are free, then about $1.50 per
// 1000).
//
//	VISION_API_KEY=... go run ./labels dog.jpg beach.png
//	(from examples/documents; needs a Google Cloud API key with the Cloud
//	Vision API enabled)
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/ubgo/sqlb"
	_ "modernc.org/sqlite" // the records tables live in in-memory SQLite

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/vision"
	"github.com/ubgo/filesystem/contrib/vision/google"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/tags"
	"github.com/ubgo/filesystem/upload"
)

// Decisions of this example, not of the library.
const (
	diskMedia = "media"
	// labelled is the one scope opted in to labelling.
	labelled = "photos"
	// keyEnv names the variable holding the API key.
	keyEnv = "VISION_API_KEY"
)

var policy = upload.Policy{MaxSize: 7 << 20, Allowed: vision.DefaultContentTypes(), Key: upload.RandomKey("photos")}

// newPlugins returns the tags plugin and the vision plugin labelling the
// labelled scope only, with labels prefixed so they never mix with tags
// people type. endpoint is "" for Google's.
// ds:def id=newplugins-wbgpbquv
func newPlugins(key, endpoint string) (*tags.Plugin, *vision.Plugin, error) {
	labeler, err := google.New(google.Config{APIKey: vision.NewSecret(key), Endpoint: endpoint})
	if err != nil {
		return nil, nil, err
	}
	t := tags.New()
	v := vision.NewPlugin(labeler, t,
		func(scope string) bool { return scope == labelled },
		vision.WithPrefix("auto:"),
		vision.WithMinScore(0.8),
		vision.WithErrorHandler(func(_ context.Context, f records.File, err error) {
			log.Printf("labelling %s failed: %v", f.Filename, err)
		}),
	)
	return t, v, nil
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: labels <image>...")
	}
	key := os.Getenv(keyEnv)
	if key == "" {
		log.Fatalf("set %s to a Google Cloud API key with the Cloud Vision API enabled", keyEnv)
	}
	if err := run(context.Background(), os.Stdout, key, "", os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

// run uploads each image at paths and prints its tags.
func run(ctx context.Context, out io.Writer, key, endpoint string, paths []string) error {
	t, v, err := newPlugins(key, endpoint)
	if err != nil {
		return err
	}
	store, err := newStore(ctx, t, v)
	if err != nil {
		return err
	}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		file, err := store.Upload(ctx, labelled, diskMedia, f, filepath.Base(p), policy)
		_ = f.Close()
		if err != nil {
			return err
		}
		got, err := t.Tags(ctx, store, labelled, file.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s  %v\n", file.Filename, got)
	}
	return nil
}

// newStore opens the records store with the plugins.
func newStore(ctx context.Context, plugins ...records.Plugin) (*records.Store, error) {
	disk, err := filesystem.NewDisk(diskMedia, memory.New())
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one connection keeps the in-memory database alive
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(plugins...))
	if err != nil {
		return nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, err
	}
	return store, nil
}
