package upload_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// inspector refuses files whose bytes contain bad, and records what it
// was shown.
type inspector struct {
	bad  string
	seen []upload.Result
}

func (in *inspector) inspect(ctx context.Context, disk *filesystem.Disk, r upload.Result) error {
	in.seen = append(in.seen, r)
	data, err := disk.ReadAll(ctx, r.Key)
	if err != nil {
		return err
	}
	if strings.Contains(string(data), in.bad) {
		return filesystem.Errorf(filesystem.CodeRejected, "inspection refused the file")
	}
	return nil
}

func TestInspectOnReceive(t *testing.T) {
	ctx := t.Context()
	disk := newDisk(t, memory.New())
	in := &inspector{bad: "EVIL"}
	p := images
	p.Inspect = in.inspect

	res, err := upload.Receive(ctx, disk, strings.NewReader(pngData), "ok.png", p)
	if err != nil || len(in.seen) != 1 || !reflect.DeepEqual(in.seen[0], res) {
		t.Fatalf("Receive = %+v, %v; inspector saw %+v", res, err, in.seen)
	}
	_, err = upload.Receive(ctx, disk, strings.NewReader(pngData+"EVIL"), "bad.png", p)
	wantCode(t, err, filesystem.CodeRejected)
	if n := countKeys(t, disk); n != 1 {
		t.Fatalf("%d objects, want only the clean one", n)
	}
}

func TestInspectOnConfirm(t *testing.T) {
	ctx := t.Context()
	disk := newDisk(t, presigning{memory.New()})
	in := &inspector{bad: "EVIL"}
	p := images
	p.Inspect = in.inspect
	ticket, err := upload.Presign(ctx, disk, upload.TicketRequest{Filename: "a.png", ContentType: "image/png", Size: 10}, p, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = disk.WriteBytes(ctx, ticket.Key, []byte(pngData+"EVIL"), filesystem.WriteOptions{}) // what the browser uploaded
	_, err = upload.Confirm(ctx, disk, ticket.Key, "a.png", p)
	wantCode(t, err, filesystem.CodeRejected)
	if exists, _ := disk.Exists(ctx, ticket.Key); exists {
		t.Fatal("a refused object was kept")
	}
}

// TestInspectDeleteFails reports both errors when the refused object
// cannot be deleted, so the caller knows it remains.
func TestInspectDeleteFails(t *testing.T) {
	ctx := t.Context()
	disk := newDisk(t, memory.New(), filesystem.WithInterceptors(func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpDelete {
			return filesystem.Errorf(filesystem.CodeUnavailable, "delete failed")
		}
		return call(ctx)
	}))
	p := images
	p.Inspect = func(context.Context, *filesystem.Disk, upload.Result) error {
		return filesystem.Errorf(filesystem.CodeRejected, "refused")
	}
	_, err := upload.Receive(ctx, disk, strings.NewReader(pngData), "a.png", p)
	if !strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("err = %v", err)
	}
}

// hashing reports a fixed SHA-256 from Stat, as a backend that stores
// checksums would.
type hashing struct{ presigning }

func (h hashing) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	e, err := h.presigning.Stat(ctx, key)
	e.SHA256 = "reported-by-the-backend"
	return e, err
}

func TestConfirmHashes(t *testing.T) {
	ctx := t.Context()
	for _, c := range []struct {
		name string
		drv  filesystem.Driver
		want string
	}{
		{"computed", presigning{memory.New()}, "dd3d6f4ffc8b8f2bf0a6b2a1ee2a7cbd2c5ba5f9a3ca1f7b88e8e5ac40de0aef"},
		{"reported", hashing{presigning{memory.New()}}, "reported-by-the-backend"},
	} {
		t.Run(c.name, func(t *testing.T) {
			disk := newDisk(t, c.drv)
			_, _ = disk.WriteBytes(ctx, "u/a.png", []byte(pngData), filesystem.WriteOptions{})
			res, err := upload.Confirm(ctx, disk, "u/a.png", "a.png", images)
			if err != nil {
				t.Fatal(err)
			}
			want := c.want
			if c.name == "computed" {
				sum := sha256.Sum256([]byte(pngData))
				want = hex.EncodeToString(sum[:])
			}
			if res.SHA256 != want {
				t.Fatalf("SHA256 = %q, want %q", res.SHA256, want)
			}
		})
	}
}

// breaksMidway serves the first bytes of an object, then fails.
type breaksMidway struct{ presigning }

func (b breaksMidway) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	rc, err := b.presigning.Read(ctx, key, rng)
	if err != nil {
		return nil, err
	}
	head, _ := io.ReadAll(io.LimitReader(rc, 600))
	_ = rc.Close()
	return io.NopCloser(io.MultiReader(bytes.NewReader(head), iotest.ErrReader(filesystem.ErrUnavailable))), nil
}

func TestConfirmReadFailsMidway(t *testing.T) {
	ctx := t.Context()
	disk := newDisk(t, breaksMidway{presigning{memory.New()}})
	_, _ = disk.WriteBytes(ctx, "u/big.png", []byte(pngData+strings.Repeat("x", 2000)), filesystem.WriteOptions{})
	_, err := upload.Confirm(ctx, disk, "u/big.png", "big.png", images)
	wantCode(t, err, filesystem.CodeUnavailable)
}
