// Command video uploads a video the way an application would with
// contrib/video: the policy removes its metadata (Sanitize) and refuses
// it when ffprobe cannot read it or it is longer than a minute (Inspect),
// and the records plugin stores its duration, size and codecs with a
// poster frame beside it. It prints what was recorded, then makes a web
// MP4 copy and a storyboard sprite for scrubbing.
//
//	go run ./video clip.mp4      (from examples/media; needs ffmpeg and ffprobe)
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/ubgo/sqlb"
	_ "modernc.org/sqlite" // the records tables live in in-memory SQLite

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

// Decisions of this example, not of the library.
const (
	diskMedia = "media"
	scope     = "demo"
	// posterAt is where the poster frame is taken.
	posterAt = time.Second
	// maxDuration is the longest video the policy accepts.
	maxDuration = time.Minute
)

// tool runs FFmpeg with its defaults: programs on PATH, one minute per
// run, at most 2 GiB copied to a work file.
var tool = video.Tool{}

// policy accepts videos up to 200 MiB, without their metadata, no longer
// than maxDuration.
// ds:def id=policy-6uud9sgf
var policy = upload.Policy{
	MaxSize: 200 << 20,
	Allowed: []string{"video/*"},
	Key:     upload.RandomKey("videos"),
	Process: tool.Sanitize(),
	Inspect: tool.Inspect(video.Limits{MaxDuration: maxDuration, MaxWidth: 3840, MaxHeight: 2160}),
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: video <file>")
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := run(context.Background(), os.Stdout, f, os.Args[1]); err != nil {
		log.Fatal(err)
	}
}

// run uploads body as filename and prints what was recorded.
func run(ctx context.Context, out io.Writer, body io.Reader, filename string) error {
	store, disk, err := newStore(ctx)
	if err != nil {
		return err
	}
	file, err := store.Upload(ctx, scope, diskMedia, body, filename, policy)
	if err != nil {
		return err
	}
	if err := report(ctx, out, disk, file); err != nil {
		return err
	}
	return derived(ctx, out, disk, file)
}

// newStore opens the records store with the video plugin installed.
// ds:def id=newstore-q787sz3p
func newStore(ctx context.Context) (*records.Store, *filesystem.Disk, error) {
	disk, err := filesystem.NewDisk(diskMedia, memory.New())
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1) // one connection keeps the in-memory database alive
	plugin := video.NewPlugin(tool, video.WithPoster(posterAt))
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(plugin))
	if err != nil {
		return nil, nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return store, disk, nil
}

// report prints the file's recorded metadata and its poster.
func report(ctx context.Context, out io.Writer, disk *filesystem.Disk, f records.File) error {
	m, ok := video.Of(f)
	if !ok {
		return fmt.Errorf("no metadata recorded for %s", f.ID)
	}
	fmt.Fprintf(out, "stored    %s (%s, %d bytes)\n", f.Key, f.ContentType, f.Size)
	fmt.Fprintf(out, "duration  %s\n", m.Duration)
	fmt.Fprintf(out, "size      %dx%d\n", m.Width, m.Height)
	fmt.Fprintf(out, "codecs    %s / %s\n", m.VideoCodec, m.AudioCodec)
	key, ok := video.PosterKey(f)
	if !ok {
		return fmt.Errorf("no poster recorded for %s", f.ID)
	}
	poster, err := disk.Stat(ctx, key)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "poster    %s (%s, %d bytes)\n", poster.Key, poster.ContentType, poster.Size)
	return nil
}
