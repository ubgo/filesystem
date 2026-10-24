package main

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
)

// mib is one mebibyte.
const mib = 1 << 20

// blockHold is how long the stand-in holds each staged block, so blocks
// staged at once overlap and the peak shows the concurrency.
const blockHold = 30 * time.Millisecond

// blockCounter is a stand-in for the Blob service that answers only what
// a Write needs (Put Block, Put Block List, Put Blob, Get Blob
// Properties) and records the size of every staged block and how many
// were in flight at once. Azurite is not needed for this step.
type blockCounter struct {
	mu       sync.Mutex
	sizes    []int64
	inflight int
	peak     int
}

func (b *blockCounter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	comp := r.URL.Query().Get("comp")
	switch {
	case r.Method == http.MethodPut && comp == "block":
		b.mu.Lock()
		b.sizes = append(b.sizes, r.ContentLength)
		b.inflight++
		b.peak = max(b.peak, b.inflight)
		b.mu.Unlock()
		time.Sleep(blockHold)
		b.mu.Lock()
		b.inflight--
		b.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPut: // Put Blob, Put Block List
		w.Header().Set("ETag", `"0x1"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodHead:
		w.Header().Set("Content-Length", "1")
		w.Header().Set("ETag", `"0x1"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("x-ms-blob-type", "BlockBlob")
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// backupDisk opens a disk for large blobs: bigger blocks raise the
// largest blob (MaxBlocks × BlockSize), and a low concurrency keeps the
// memory a Write holds (Concurrency × BlockSize) in check.
// ds:def id=backupdisk-f6m2x9kr
func backupDisk(client *azblob.Client, blockSize int64, concurrency int) (*filesystem.Disk, error) {
	drv, err := azure.New(azure.Config{
		Client:      client,
		Container:   "backups",
		BlockSize:   blockSize,   // 0 is DefaultBlockSize (8 MiB)
		Concurrency: concurrency, // 0 is DefaultConcurrency (5)
	})
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk("backups", drv)
}

// largeBlobs writes one body through the default settings and through a
// tuned disk against the stand-in, printing the blocks each staged, then
// shows the bounds New refuses.
// ds:def id=largeblobs-n4w8c3tp
func largeBlobs(ctx context.Context, out io.Writer) error {
	for _, c := range []struct {
		label       string
		blockSize   int64
		concurrency int
		body        int64
	}{
		{"defaults", 0, 0, 9 * mib},
		{"2 MiB blocks, one at a time", 2 * mib, 1, 5 * mib},
		{"1 MiB blocks, three at once", 1 * mib, 3, 9 * mib},
	} {
		counter := &blockCounter{}
		srv := httptest.NewServer(counter)
		client, err := azblob.NewClientWithNoCredential(srv.URL+"/", &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
		if err != nil {
			srv.Close()
			return err
		}
		disk, err := backupDisk(client, c.blockSize, c.concurrency)
		if err != nil {
			srv.Close()
			return err
		}
		_, err = disk.Write(ctx, "db/2026-10-09.dump", bytes.NewReader(make([]byte, c.body)), filesystem.WriteOptions{})
		srv.Close()
		if err != nil {
			return err
		}
		counter.mu.Lock()
		staged := slices.Clone(counter.sizes)
		peak := counter.peak
		counter.mu.Unlock()
		// Blocks staged at once arrive in any order; the last block is the
		// remainder, so print them largest first.
		slices.SortFunc(staged, func(a, b int64) int { return cmp.Compare(b, a) })
		sizes := make([]string, len(staged))
		for i, n := range staged {
			sizes[i] = fmt.Sprint(n / mib)
		}
		limit := c.concurrency
		if limit == 0 {
			limit = azure.DefaultConcurrency
		}
		fmt.Fprintf(out, "%s: %d MiB staged as %d blocks (MiB: %s), at most %d in flight: %t\n",
			c.label, c.body/mib, len(sizes), strings.Join(sizes, ","), limit, peak <= limit)
	}
	client, err := azblob.NewClientWithNoCredential("http://127.0.0.1:1/", nil)
	if err != nil {
		return err
	}
	for _, c := range []struct {
		label       string
		blockSize   int64
		concurrency int
	}{{"512 KiB blocks", azure.MinBlockSize / 2, 0}, {"4001 MiB blocks", azure.MaxBlockSize + mib, 0}, {"negative concurrency", 0, -1}} {
		_, err := backupDisk(client, c.blockSize, c.concurrency)
		fmt.Fprintf(out, "%s: %s\n", c.label, filesystem.CodeOf(err))
	}
	return nil
}
