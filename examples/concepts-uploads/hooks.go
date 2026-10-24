package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// blockedMarker is what the demo inspector refuses; a real one scans
// with ClamAV (package scan) or decodes the image fully.
const blockedMarker = "blocked"

// abandonedAfter is how old an unconfirmed upload must be to be swept.
const abandonedAfter = 24 * time.Hour

// trimText is a Processor: it rewrites plain text before it is stored
// (here, trimming surrounding space) and passes everything else through.
// What it returns is checked against the policy again.
// ds:def id=trimtext-5f5xcut2
func trimText(_ context.Context, in upload.Info, body io.Reader) (io.Reader, upload.Info, error) {
	if !strings.HasPrefix(in.ContentType, "text/plain") {
		return body, in, nil
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return nil, upload.Info{}, err
	}
	return bytes.NewReader(bytes.TrimSpace(b)), in, nil
}

// refuseMarked is an Inspector: it reads the stored file and refuses it
// with CodeRejected, which makes the upload delete the object.
// ds:def id=refusemarked-3nej68ju
func refuseMarked(ctx context.Context, disk *filesystem.Disk, r upload.Result) error {
	b, err := disk.ReadAll(ctx, r.Key)
	if err != nil {
		return err
	}
	if bytes.Contains(b, []byte(blockedMarker)) {
		return filesystem.Errorf(filesystem.CodeRejected, "inspection refused %s", r.Filename)
	}
	return nil
}

// processAndInspect runs both hooks: Process before storing, Inspect
// after storing and before success is reported.
// ds:def id=processandinspect-2ryw5smr
func processAndInspect(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	p := upload.Policy{MaxSize: maxUpload, Allowed: []string{"text/plain"}, Process: trimText, Inspect: upload.Inspectors(refuseMarked)}
	res, err := upload.Receive(ctx, disk, strings.NewReader("  notes  \n"), "notes.txt", p)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "processed: %d bytes\n", res.Size)
	_, err = upload.Receive(ctx, disk, strings.NewReader("this is blocked"), "bad.txt", p)
	fmt.Fprintf(out, "inspected: %s, objects stored: %d\n", filesystem.CodeOf(err), count(ctx, disk))
	return nil
}

// sweep deletes uploads under a directory that are older than a cutoff:
// tickets nobody confirmed, files nobody attached. The memory driver's
// clock is set back a day so one object is old.
// ds:def id=sweep-52zp4ugj
func sweep(ctx context.Context, out io.Writer) error {
	now := time.Now()
	clock := now.Add(-2 * abandonedAfter)
	disk, err := filesystem.NewDisk("uploads", memory.New(memory.WithClock(func() time.Time { return clock })))
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "incoming/old.png", []byte(pngFile), filesystem.WriteOptions{}); err != nil {
		return err
	}
	clock = now
	if _, err := disk.WriteBytes(ctx, "incoming/new.png", []byte(pngFile), filesystem.WriteOptions{}); err != nil {
		return err
	}
	n, err := upload.Sweep(ctx, disk, "incoming", now.Add(-abandonedAfter))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "swept %d, left %d\n", n, count(ctx, disk))
	_, err = upload.Sweep(ctx, disk, "", now)
	fmt.Fprintf(out, "sweep the root: %s\n", filesystem.CodeOf(err))
	return nil
}
