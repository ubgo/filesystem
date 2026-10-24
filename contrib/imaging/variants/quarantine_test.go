package variants_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/upload"
)

// TestQuarantinedImage: a quarantined image is recorded without its eager
// variants, and no path generates one from its unscanned bytes (Get, URL,
// Generate, GenerateMissing); the scan passing generates them, and a
// replacement quarantined again drops them until its own release.
func TestQuarantinedImage(t *testing.T) {
	ctx := t.Context()
	db, _ := recordstest.Open(t)
	disk, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithURLSigner(signer{}))
	mgr, _ := filesystem.NewManager(disk)
	p, err := variants.New(transforms, imaging.Options{}, variants.WithEager(nil, "thumb"))
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
	images := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}}
	f, err := s.Upload(ctx, "t", "media", bytes.NewReader(pngBytes(t, 80, 80)), "a.png", images, records.Quarantine())
	if err != nil || !f.Quarantined() {
		t.Fatalf("a quarantined upload with eager variants = %+v, %v", f, err)
	}
	if rows() != 0 {
		t.Fatalf("%d variants of a quarantined image", rows())
	}
	quarantined := func(err error) {
		t.Helper()
		if !errors.Is(err, records.ErrQuarantined) {
			t.Fatalf("err = %v, want ErrQuarantined", err)
		}
	}
	_, err = p.Get(ctx, s, "t", f.ID, "thumb")
	quarantined(err)
	_, err = p.URL(ctx, s, "t", f.ID, "thumb", time.Minute)
	quarantined(err)
	quarantined(p.Generate(ctx, s, "t", f.ID))
	res, err := p.GenerateMissing(ctx, s, variants.MissingOptions{Scope: "t"})
	if err != nil || res.Generated != 0 || len(res.Failed) != 0 || res.Complete != 0 {
		t.Fatalf("GenerateMissing over a quarantined image = %+v, %v", res, err)
	}
	if rows() != 0 {
		t.Fatalf("%d variants generated while quarantined", rows())
	}

	got, err := s.PassScan(ctx, "t", f.ID)
	if err != nil || got.Status != records.StatusReady || rows() != 1 {
		t.Fatalf("after release: %+v, %v, %d variants", got, err, rows())
	}
	if _, err := p.URL(ctx, s, "t", f.ID, "thumb", time.Minute); err != nil {
		t.Fatalf("URL after release: %v", err)
	}

	if _, err := s.Replace(ctx, "t", f.ID, bytes.NewReader(pngBytes(t, 60, 30)), "", images, records.QuarantineContent()); err != nil {
		t.Fatal(err)
	}
	if rows() != 0 {
		t.Fatalf("%d variants kept or made for quarantined new content", rows())
	}
	if _, err := s.PassScan(ctx, "t", f.ID); err != nil || rows() != 1 {
		t.Fatalf("after the second release: %v, %d variants", err, rows())
	}
}

// TestQuarantinedDuringGeneration: an image quarantined (its content
// replaced with QuarantineContent) while one of its variants is being
// generated is not generated again from the new, unscanned bytes: the
// retry finds it quarantined, and the stale variant is not recorded.
func TestQuarantinedDuringGeneration(t *testing.T) {
	ctx := t.Context()
	r := newRaceEnv(t)
	f := r.image(t, "a.png", pngBytes(t, 80, 80), filesystem.VisibilityPrivate)
	images := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}}
	r.once(func() {
		if _, err := r.store.Replace(ctx, "t", f.ID, bytes.NewReader(pngBytes(t, 40, 40)), "", images, records.QuarantineContent()); err != nil {
			t.Error(err)
		}
	})
	if _, err := r.plugin.Get(ctx, r.store, "t", f.ID, "thumb"); !errors.Is(err, records.ErrQuarantined) {
		t.Fatalf("Get across a quarantine = %v", err)
	}
	var n int
	_ = r.db.QueryRow("SELECT COUNT(*) FROM " + variants.TableNameVariants).Scan(&n)
	if n != 0 {
		t.Fatalf("%d variants recorded", n)
	}
}
