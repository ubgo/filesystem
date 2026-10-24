//go:build unix

package office_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/office"
)

// TestConvertToPDF_TimeoutKillsEveryProcess pins that a run past Timeout
// is stopped with the processes it started, as soffice starts
// soffice.bin.
func TestConvertToPDF_TimeoutKillsEveryProcess(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	work := t.TempDir()
	tool := office.Tool{SOffice: fakeSOffice(t, "sleep 300 & echo $! > "+pidFile+"; wait"), Timeout: 5 * time.Second, TempDir: work}
	start := time.Now()
	_, err := tool.ConvertToPDF(t.Context(), strings.NewReader("x"), office.DOCX)
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable || time.Since(start) > 10*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d, started by the program, outlived the timeout", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
	emptyDir(t, work)
}
