package pdf_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/pdf"
	"github.com/ubgo/filesystem/records"
)

// sweepAll runs SweepOrphans over the whole of disk, every object old
// enough, and returns how many it deleted.
func sweepAll(t *testing.T, s *records.Store, disk string) int {
	t.Helper()
	n, err := s.SweepOrphans(t.Context(), disk, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// existsOn reports whether key is on d.
func existsOn(t *testing.T, d *filesystem.Disk, key string) bool {
	t.Helper()
	ok, err := d.Exists(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// uploadDoc records a one-page text PDF and returns it as Process left it.
func uploadDoc(t *testing.T, e *env) records.File {
	t.Helper()
	f, err := e.store.Upload(t.Context(), scope, "media", bytes.NewReader(textDocument("", "Revenue grew")), "r.pdf", docPolicy)
	if err != nil {
		t.Fatal(err)
	}
	return e.reread(t, f)
}

// TestOwnedObjects_SweepKeepsDerivatives pins the ObjectOwner:
// SweepOrphans never deletes a preview or text whose file row (live or
// trashed) names it, on the file's disk or on a WithLocation disk, and
// still deletes one whose file is gone.
func TestOwnedObjects_SweepKeepsDerivatives(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	previewOnCDN := func(f records.File, name string) (string, string, error) {
		if name == pdf.DerivedPreview {
			return "cdn", "previews/" + f.ID + "-", nil
		}
		return "", f.Key + pdf.TextInfix, nil
	}
	for name, opts := range map[string][]pdf.PluginOption{
		"default":  {pdf.WithPreview(), pdf.WithText()},
		"location": {pdf.WithPreview(), pdf.WithText(), pdf.WithLocation(previewOnCDN)},
	} {
		t.Run(name, func(t *testing.T) {
			e, cdn := twoDiskEnv(t, pdf.NewPlugin(pdf.Tool{}, opts...))
			disks := map[string]*filesystem.Disk{"media": e.disk, "cdn": cdn}
			f := uploadDoc(t, e)
			pDisk, pKey, ok1 := pdf.PreviewLocation(f)
			tDisk, tKey, ok2 := pdf.TextLocation(f)
			if !ok1 || !ok2 {
				t.Fatalf("derivatives missing: %v", f.Metadata)
			}
			for _, d := range []string{"media", "cdn"} {
				if n := sweepAll(t, e.store, d); n != 0 {
					t.Fatalf("the sweep of %s deleted %d objects of a live document", d, n)
				}
			}
			if err := e.store.Trash(t.Context(), scope, f.ID); err != nil {
				t.Fatal(err)
			}
			for _, d := range []string{"media", "cdn"} {
				if n := sweepAll(t, e.store, d); n != 0 {
					t.Fatalf("the sweep of %s deleted %d objects of a trashed document", d, n)
				}
			}
			if !existsOn(t, disks[pDisk], pKey) || !existsOn(t, disks[tDisk], tKey) {
				t.Fatal("a derivative went")
			}
			// Derivatives left behind after the purge are orphans.
			if err := e.store.Purge(t.Context(), scope, f.ID); err != nil {
				t.Fatal(err)
			}
			for _, o := range [][2]string{{pDisk, pKey}, {tDisk, tKey}} {
				if _, err := disks[o[0]].WriteBytes(t.Context(), o[1], []byte("left behind"), filesystem.WriteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if sweepAll(t, e.store, "media")+sweepAll(t, e.store, "cdn") != 2 || existsOn(t, disks[pDisk], pKey) || existsOn(t, disks[tDisk], tKey) {
				t.Fatal("an orphaned derivative survived the sweep")
			}
		})
	}
}

// TestMoveFileDisk_MovesTheDerivatives pins the FileDiskMover: MoveFile
// takes the preview and text on the file's disk along, leaves one
// WithLocation put on a disk of its own, and a move that fails part way
// leaves nothing on the target disk.
func TestMoveFileDisk_MovesTheDerivatives(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	e, cdn := twoDiskEnv(t, pdf.NewPlugin(pdf.Tool{}, pdf.WithPreview(), pdf.WithText()))
	f := uploadDoc(t, e)
	_, pKey, _ := pdf.PreviewLocation(f)

	moved, err := e.store.MoveFile(t.Context(), scope, f.ID, "cdn")
	if err != nil {
		t.Fatal(err)
	}
	for _, loc := range []func(records.File) (string, string, bool){pdf.PreviewLocation, pdf.TextLocation} {
		if disk, key, _ := loc(moved); disk != "cdn" || !existsOn(t, cdn, key) || existsOn(t, e.disk, key) {
			t.Fatalf("%s:%s after the move (left on media %v)", disk, key, existsOn(t, e.disk, key))
		}
	}
	if _, k, _ := pdf.PreviewLocation(moved); k != pKey {
		t.Fatalf("the preview's key changed: %q", k)
	}
	// A text gone from the disk fails the move after the preview was
	// copied: the copy is deleted and the file stays where it was.
	g := uploadDoc(t, e)
	_, gPreview, _ := pdf.PreviewLocation(g)
	_, gText, _ := pdf.TextLocation(g)
	if err := e.disk.Delete(t.Context(), gText); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.MoveFile(t.Context(), scope, g.ID, "cdn"); err == nil {
		t.Fatal("a move with a missing text succeeded")
	}
	if existsOn(t, cdn, gPreview) || existsOn(t, cdn, g.Key) || !existsOn(t, e.disk, gPreview) {
		t.Fatal("a failed move left copies behind or lost the preview")
	}
	if cur := e.reread(t, g); cur.Disk != "media" {
		t.Fatalf("the file moved to %q", cur.Disk)
	}
}

// TestMoveFileDisk_LeavesLocatedDerivatives: a preview WithLocation put
// on a disk of its own stays there when its file moves.
func TestMoveFileDisk_LeavesLocatedDerivatives(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	loc := func(f records.File, name string) (string, string, error) {
		if name == pdf.DerivedPreview {
			return "cdn", "previews/" + f.ID + "-", nil
		}
		return "", f.Key + pdf.TextInfix, nil
	}
	e, cdn := twoDiskEnv(t, pdf.NewPlugin(pdf.Tool{}, pdf.WithPreview(), pdf.WithText(), pdf.WithLocation(loc)))
	f := uploadDoc(t, e)
	_, pKey, _ := pdf.PreviewLocation(f)
	moved, err := e.store.MoveFile(t.Context(), scope, f.ID, "cdn")
	if err != nil {
		t.Fatal(err)
	}
	if disk, key, _ := pdf.PreviewLocation(moved); disk != "cdn" || key != pKey || !existsOn(t, cdn, pKey) {
		t.Fatalf("the located preview at %s:%s", disk, key)
	}
	if disk, key, _ := pdf.TextLocation(moved); disk != "cdn" || !existsOn(t, cdn, key) || existsOn(t, e.disk, key) {
		t.Fatalf("the text at %s:%s", disk, key)
	}
}
