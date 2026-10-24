package main

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/office"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
)

// TestRenditionRefusesOtherTypes pins that only office documents are
// converted.
func TestRenditionRefusesOtherTypes(t *testing.T) {
	disk, err := filesystem.NewDisk(diskMedia, memory.New())
	if err != nil {
		t.Fatal(err)
	}
	_, err = keepPDFRendition(t.Context(), disk, records.File{Key: "a.png", ContentType: "image/png"})
	if filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Fatalf("rendition of a PNG = %v, want unsupported", err)
	}
}

// TestRenditionWithoutLibreOffice pins the error on a server without
// LibreOffice: it wraps office.ErrNoProgram, so an app can tell a missing
// program from a broken document. It runs only where soffice is absent.
func TestRenditionWithoutLibreOffice(t *testing.T) {
	if _, err := exec.LookPath("soffice"); err == nil {
		t.Skip("soffice is installed")
	}
	disk, err := filesystem.NewDisk(diskMedia, memory.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := disk.Write(t.Context(), "doc.docx", strings.NewReader("PK\x03\x04"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = keepPDFRendition(t.Context(), disk, records.File{Key: "doc.docx", ContentType: office.DOCX})
	if !errors.Is(err, office.ErrNoProgram) {
		t.Fatalf("rendition without soffice = %v, want office.ErrNoProgram", err)
	}
}
