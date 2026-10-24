package sftp_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"

	"github.com/ubgo/filesystem"
	fssftp "github.com/ubgo/filesystem/adapters/sftp"
	"github.com/ubgo/filesystem/filesystemtest"
)

// pipeClient connects an SFTP client to an in-process server over pipes.
func pipeClient(t *testing.T, opts ...sftp.ServerOption) *sftp.Client {
	t.Helper()
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	server, err := sftp.NewServer(struct {
		io.Reader
		io.WriteCloser
	}{c2sR, s2cW}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve() }()
	client, err := sftp.NewClientPipe(s2cR, c2sW)
	if err != nil {
		t.Fatal(err)
	}
	// The server closes first: the client's Close waits for its reader,
	// which ends only when the server side of the pipe is closed.
	t.Cleanup(func() { _ = server.Close(); _ = c2sR.Close(); _ = client.Close() })
	return client
}

func newDisk(t *testing.T) *filesystem.Disk {
	t.Helper()
	drv, err := fssftp.New(fssftp.Config{Client: pipeClient(t), Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("sftp", drv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestConformance(t *testing.T) {
	filesystemtest.Run(t, newDisk)
}

// TestCaseProbeMatchesHost pins the probe New runs: the in-process server
// writes to a local temp dir, so the driver must declare exactly what the
// host filesystem does (folding on macOS APFS, distinct on Linux).
func TestCaseProbeMatchesHost(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probe"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(dir, "PROBE"))
	hostFolds := err == nil
	drv, err := fssftp.New(fssftp.Config{Client: pipeClient(t), Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if got := drv.Capabilities().CaseInsensitive; got != hostFolds {
		t.Fatalf("CaseInsensitive = %v, host filesystem folds case: %v", got, hostFolds)
	}
	left, err := os.ReadDir(filepath.Join(dir, fssftp.StagingDir, "tmp"))
	if err != nil || len(left) != 0 {
		t.Fatalf("probe left %v behind (%v)", left, err)
	}
}
