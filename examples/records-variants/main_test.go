package main

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/records"
)

// testStore returns a migrated Store with the variants plugin.
func testStore(t *testing.T) (*records.Store, *variants.Plugin) {
	t.Helper()
	db, err := openSQLite(filepath.Join(t.TempDir(), "records.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	disks, err := newDisks()
	if err != nil {
		t.Fatal(err)
	}
	p, err := newPlugin(transforms, func(_ context.Context, _ records.File, name string, err error) {
		t.Errorf("eager %s: %v", name, err)
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := newStore(t.Context(), db, disks, p)
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"variant objects after upload: 1", "card: 400x267 image/jpeg", "GET thumb: 302 to " + signBase,
		"GET huge: 404", "variant objects after replace: 1", "card after replace: 267x400",
		"public thumb: " + cdnBase + "/variants/", "pruned after the card transform changed: 1", "variant objects after purge: 0",
		`focal point set: banner remade true`,
		`catch-up: generated 2, complete 0, failed 0`,
		`events: variants.ready [avatar,avatar-2x,avatar-3x,banner], variants.ready [avatar,avatar-2x,avatar-3x,banner], variants.ready [avatar,avatar-2x,avatar-3x,banner]`,
		`density srcset descriptors: 1x 2x 3x`,
		`GET banner (Accept "image/png,image/*;q=0.8"): 302 banner….png, Vary "Accept, Sec-CH-DPR, DPR"`,
		`GET banner (Accept "image/*"): 302 banner….jpg, Vary "Accept, Sec-CH-DPR, DPR"`,
		`GET avatar?dpr=2.5 (Accept ""): 302 avatar-3x….jpg, Vary "Accept, Sec-CH-DPR, DPR"`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestGetIsCached(t *testing.T) {
	s, p := testStore(t)
	f, err := uploadPhoto(t.Context(), s, 300, 200)
	if err != nil {
		t.Fatal(err)
	}
	a, err := card(t.Context(), s, p, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := card(t.Context(), s, p, f.ID)
	if err != nil || a.ID != b.ID {
		t.Fatalf("second Get = %+v, %v; want the cached %s", b, err, a.ID)
	}
}

func TestNotAnImage(t *testing.T) {
	s, p := testStore(t)
	disk, _ := s.Disks().Disk(diskName)
	if _, err := disk.Write(t.Context(), "notes.txt", bytes.NewReader([]byte("plain text")), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := s.CreateFile(t.Context(), records.NewFile{Scope: tenant, Disk: diskName, Key: "notes.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := card(t.Context(), s, p, f.ID); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("variant of a text file = %v, want rejected", err)
	}
}

func TestHandlerRefusesUndeclaredNames(t *testing.T) {
	s, p := testStore(t)
	f, err := uploadPhoto(t.Context(), s, 300, 200)
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(s, p)
	if code, _ := fetch(h, "/variants/"+f.ID+"/400x400"); code != http.StatusNotFound {
		t.Fatalf("undeclared name = %d", code)
	}
	if code, _ := fetch(h, "/variants/"+f.ID+"/"+nameCard); code != http.StatusFound {
		t.Fatalf("declared name = %d", code)
	}
}

func TestTrashedFileGetsNoNewVariants(t *testing.T) {
	s, p := testStore(t)
	f, err := uploadPhoto(t.Context(), s, 300, 200)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Trash(t.Context(), tenant, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := card(t.Context(), s, p, f.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("variant of a trashed file = %v", err)
	}
}

// TestVariantsOnAnotherDisk pins WithLocation: the variant is on the
// thumbs disk, none is on the file's own, and the purge removes it there.
func TestVariantsOnAnotherDisk(t *testing.T) {
	var out strings.Builder
	if err := otherDisk(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"located thumb: disk thumbs, key under thumbs/acme/{file}/: true",
		"variant objects: 0 on media, 1 on thumbs",
		"thumb link: https://app.example.com/files/thumbs/thumbs/…",
		"after purge: 0 variant objects on thumbs",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestLocationOnUnknownDiskFails pins that a location naming a disk the
// Manager lacks fails the request with nothing written.
func TestLocationOnUnknownDiskFails(t *testing.T) {
	ctx := t.Context()
	db, err := openSQLite(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	disks, err := newSplitDisks()
	if err != nil {
		t.Fatal(err)
	}
	p, err := variants.New(transforms, imaging.Options{}, variants.WithLocation(func(f records.File, _ string) (string, string, error) {
		return "missing", "x/", nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	s, err := newStore(ctx, db, disks, p)
	if err != nil {
		t.Fatal(err)
	}
	photo, err := uploadPhoto(ctx, s, 64, 64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get(ctx, s, tenant, photo.ID, nameThumb); err == nil {
		t.Fatal("Get on an unknown disk succeeded")
	}
	// The original is the only object on either disk.
	for d, want := range map[string]int{diskName: 1, thumbsDisk: 0} {
		if n, err := objectsOn(ctx, disks, d, ""); err != nil || n != want {
			t.Fatalf("%s holds %d objects (%v) after a failed Get, want %d", d, n, err, want)
		}
	}
}

// TestProxyRanges pins ModeProxy's serve path: a whole thumbnail with the
// security headers and no redirect, a Range answered 206 with the first
// bytes of the same body, its ETag answered 304, and an unknown mode 500.
func TestProxyRanges(t *testing.T) {
	var out strings.Builder
	if err := proxied(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`proxy GET thumb: 200 image/jpeg, 4008 bytes, Location "", nosniff true, CSP set true, Cache-Control "public, max-age=86400"`,
		"proxy GET thumb (Range bytes=0-99): 206, 100 bytes, Content-Range bytes 0-99/4008, same bytes as the full body true",
		"proxy GET thumb (If-None-Match its ETag): 304, 0 bytes",
		`handler with mode "stream": 500`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
