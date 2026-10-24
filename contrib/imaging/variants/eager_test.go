package variants_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/upload"
)

func TestEager(t *testing.T) {
	ctx := t.Context()
	db, _ := recordstest.Open(t)
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	var failed []string
	p, err := variants.New(transforms, imaging.Options{}, variants.WithEager(func(_ context.Context, f records.File, name string, err error) {
		failed = append(failed, f.Key+"/"+name)
	}, "thumb"))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(p))
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rows := func() int {
		var n int
		_ = db.QueryRow("SELECT COUNT(*) FROM " + variants.TableNameVariants).Scan(&n)
		return n
	}
	record := func(key string, data []byte, ct string) records.File {
		_, _ = disk.WriteBytes(ctx, key, data, filesystem.WriteOptions{})
		f, err := s.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: key, ContentType: ct})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	img := record("a.png", pngBytes(t, 80, 80), "")
	if rows() != 1 {
		t.Fatalf("%d variants after recording an image, want the eager thumb", rows())
	}
	record("notes.txt", []byte("hello"), "")
	record("broken.png", []byte("\x89PNG\r\n\x1a\nbroken"), "image/png")
	if rows() != 1 || len(failed) != 1 || failed[0] != "broken.png/thumb" {
		t.Fatalf("rows %d, failures %v", rows(), failed)
	}
	anything := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}}
	if _, err := s.Replace(ctx, "t", img.ID, bytes.NewReader(pngBytes(t, 120, 60)), "", anything); err != nil {
		t.Fatal(err)
	}
	_, err = p.Get(ctx, s, "t", img.ID, "thumb")
	if err != nil || rows() != 1 {
		t.Fatalf("after replace: %d rows, %v", rows(), err)
	}

	// A trashed image loses its variants; restoring it makes the eager
	// ones again at once.
	if err := s.Trash(ctx, "t", img.ID); err != nil || rows() != 0 {
		t.Fatalf("after trash: %d rows, %v", rows(), err)
	}
	if err := s.Restore(ctx, "t", img.ID); err != nil || rows() != 1 {
		t.Fatalf("after restore: %d rows, %v", rows(), err)
	}
	// An image trashed in the transaction that recorded it is not warmed.
	_, _ = disk.WriteBytes(ctx, "c.png", pngBytes(t, 40, 40), filesystem.WriteOptions{})
	err = s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		f, err := s.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "c.png"})
		if err != nil {
			return err
		}
		return s.Trash(ctx, "t", f.ID)
	})
	if err != nil || rows() != 1 || len(failed) != 1 {
		t.Fatalf("trashed at once: %v, %d rows, failures %v", err, rows(), failed)
	}

	// A rolled-back recording makes nothing.
	boom := errors.New("later step failed")
	_, _ = disk.WriteBytes(ctx, "b.png", pngBytes(t, 40, 40), filesystem.WriteOptions{})
	err = s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := s.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "b.png"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || rows() != 1 {
		t.Fatalf("rollback: %v, %d rows", err, rows())
	}

	// A replacement whose old variants cannot be dropped fails whole.
	if _, err := db.Exec("DROP TABLE " + variants.TableNameVariants); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Replace(ctx, "t", img.ID, bytes.NewReader(pngBytes(t, 10, 10)), "", anything); err == nil {
		t.Fatal("a replacement went through without dropping the variants")
	}

	if _, err := variants.New(transforms, imaging.Options{}, variants.WithEager(nil, "huge")); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("an undeclared eager variant = %v", err)
	}
}
