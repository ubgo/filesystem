//go:build unix

package office_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/office"
)

// TestDefaultProgramIsLookedUpOnPath pins that an empty Tool.SOffice means
// DefaultSOffice looked up on PATH, missing there being ErrNoProgram.
func TestDefaultProgramIsLookedUpOnPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := office.Tool{}.ConvertToPDF(t.Context(), strings.NewReader("x"), office.DOCX)
	if !errors.Is(err, office.ErrNoProgram) || !strings.Contains(err.Error(), office.DefaultSOffice) {
		t.Fatalf("got %v, want ErrNoProgram naming %s", err, office.DefaultSOffice)
	}
}

// TestFailingInputIsReturned pins that a document whose reader fails is
// that read error, the program never run and nothing left behind.
func TestFailingInputIsReturned(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	tool := office.Tool{SOffice: fakeSOffice(t, "touch "+marker), TempDir: t.TempDir()}
	broken := errors.New("upload connection reset")
	if _, err := tool.ConvertToPDF(t.Context(), iotest.ErrReader(broken), office.DOCX); !errors.Is(err, broken) {
		t.Fatalf("got %v, want the reader's error", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the program ran on a document that was never read whole")
	}
	emptyDir(t, tool.TempDir)
}

// TestProgramThatCannotStart pins that an executable the system cannot
// run (no binary format, no "#!") fails the conversion.
func TestProgramThatCannotStart(t *testing.T) {
	prog := filepath.Join(t.TempDir(), "soffice")
	if err := os.WriteFile(prog, []byte{0, 1, 2, 3}, 0o700); err != nil {
		t.Fatal(err)
	}
	tool := office.Tool{SOffice: prog, TempDir: t.TempDir()}
	if _, err := tool.ConvertToPDF(t.Context(), strings.NewReader("x"), office.DOCX); err == nil {
		t.Fatal("a program that cannot start converted the document")
	}
	emptyDir(t, tool.TempDir)
}

// TestCallerCancellation pins that a conversion the caller cancels (not
// its own timeout) stops the program and is CodeUnavailable.
func TestCallerCancellation(t *testing.T) {
	tool := office.Tool{SOffice: fakeSOffice(t, "sleep 30"), TempDir: t.TempDir(), Timeout: time.Minute}
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, err := tool.ConvertToPDF(ctx, strings.NewReader("x"), office.DOCX)
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want CodeUnavailable wrapping context.Canceled", err)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatal("the program was not stopped when the caller cancelled")
	}
	emptyDir(t, tool.TempDir)
}
