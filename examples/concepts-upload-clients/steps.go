package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/upload"
)

// replayWindow is how long a recorded response is replayed: longer than
// the longest a client's offline queue keeps retrying one file.
const replayWindow = 24 * time.Hour

// randomKey matches the random part of a stored key, so the output of a
// JSON body stays the same from run to run.
var randomKey = regexp.MustCompile(`uploads/[a-z0-9]+\.png`)

// stable replaces random keys and digests in a JSON body with markers.
func stable(body string) string {
	body = randomKey.ReplaceAllString(body, "uploads/<random>.png")
	return regexp.MustCompile(`"sha256":"[0-9a-f]{64}"`).ReplaceAllString(strings.TrimSpace(body), `"sha256":"<hex>"`)
}

// jsonContract shows the two JSON shapes every upload endpoint answers
// with: upload.Response on success (one entry per stored file) and
// upload.ErrorResponse on failure, whose code tells "too large" from
// "wrong type" without parsing the message.
// ds:def id=jsoncontract-7ek5apjw
func jsonContract(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	h := upload.Handler(disk, policy, upload.HandlerOptions{})
	ok, err := post(ctx, h, nil, file("photo.png", photo))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "success %d: %s\n", ok.Code, stable(ok.Body.String()))
	bad, err := post(ctx, h, nil, file("page.png", "<!DOCTYPE html><p>hi</p>"))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "failure %d: %s\n", bad.Code, strings.TrimSpace(bad.Body.String()))
	return nil
}

// chunked sends one file the way Dropzone does: three POSTs, each with the
// upload ID, the chunk's index, the chunk count and the total size before
// the chunk's bytes. Chunks arrive out of order and one is sent twice;
// the request that completes the set answers 201 with the stored file.
// ds:def id=chunked-x4e8fmh9
func chunked(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	h, err := upload.Chunked(disk, policy, upload.ChunkedOptions{Staging: staging, Owner: owner})
	if err != nil {
		return err
	}
	f := upload.DropzoneFields()
	send := func(index int) (int, string, error) {
		chunk := photo[index*chunkSize : min((index+1)*chunkSize, len(photo))]
		rec, err := post(ctx, h, nil,
			part{field: f.UploadID, value: "4f1c-photo"},
			part{field: f.Index, value: itoa(index)},
			part{field: f.Total, value: itoa((len(photo) + chunkSize - 1) / chunkSize)},
			part{field: f.TotalSize, value: itoa(len(photo))},
			part{field: f.File, file: "photo.png", value: chunk},
		)
		if err != nil {
			return 0, "", err
		}
		return rec.Code, stable(rec.Body.String()), nil
	}
	for _, index := range []int{0, 2, 2, 1} {
		code, body, err := send(index)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "chunk %d: %d %s\n", index, code, body)
	}
	return nil
}

// progress tells the server how far a file has streamed to the disk,
// about every upload.ProgressStep bytes and once with the total, for a
// progress bar fed over a WebSocket or a log of slow uploads.
// ds:def id=progress-cy4jj5kf
func progress(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	var calls, last atomic.Int64
	p := policy
	p.Progress = func(_ context.Context, _ upload.Info, received int64) {
		calls.Add(1)
		last.Store(received)
	}
	big := pngHeader + strings.Repeat("x", 3*upload.ProgressStep)
	res, err := upload.Receive(ctx, disk, strings.NewReader(big), "big.png", p)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "progress: %d calls, last %d of %d bytes\n", calls.Load(), last.Load(), res.Size)
	return nil
}

// folder accepts a dropped folder: each file arrives with its relative
// path as its name, SafePath keeps it made safe (a ".." cannot climb out),
// and FolderKey stores each file at that path, so the folder keeps its
// layout. Rename on collision keeps both of two files with one path.
// ds:def id=folder-gns7cy6w
func folder(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	p := policy
	p.Key = upload.FolderKey("users/" + demoUser + "/drive")
	p.Collision = upload.CollisionRename
	h := upload.Handler(disk, p, upload.HandlerOptions{MaxFiles: 4})
	rec, err := post(ctx, h, nil,
		file("Trip/day1/beach.png", photo),
		file("Trip/day2/hike.png", photo),
		file("Trip/../../../etc/cron.png", photo),
		file("Trip/day1/beach.png", photo),
	)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "folder upload: %d\n", rec.Code)
	for e, err := range disk.List(ctx, "users", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "  %s\n", e.Key)
	}
	return nil
}

// idempotent makes retries safe: a client sends one Idempotency-Key per
// file on every attempt, the first request runs, and each retry gets the
// recorded response back without storing the file again. The same key
// with another file is refused with 422.
// ds:def id=idempotent-hfeg247q
func idempotent(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	store, err := upload.NewMemoryIdempotency(upload.MemoryIdempotencyOptions{TTL: replayWindow})
	if err != nil {
		return err
	}
	h := upload.Handler(disk, policy, upload.HandlerOptions{
		Idempotency: &upload.Idempotency{Store: store, Owner: owner},
	})
	key := http.Header{upload.HeaderIdempotencyKey: {"9b2e7c0a-queued-photo-1"}}
	for attempt, body := range []string{photo, photo, photo + "changed"} {
		rec, err := post(ctx, h, key, file("photo.png", body))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "attempt %d: %d replayed=%q\n", attempt+1, rec.Code, rec.Header().Get(upload.HeaderIdempotentReplayed))
	}
	n := 0
	for _, err := range disk.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		n++
	}
	fmt.Fprintf(out, "objects stored: %d\n", n)
	return nil
}
