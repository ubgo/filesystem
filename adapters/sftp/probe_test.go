package sftp_test

import (
	"context"
	"errors"
	"io"
	"path"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pkg/sftp"

	fssftp "github.com/ubgo/filesystem/adapters/sftp"
)

// errProbe is the failure the faulty handlers below answer the case
// probe with; the SFTP server sends it as a generic failure.
var errProbe = errors.New("probe refused")

// isProbe reports whether p is a file the case probe creates or looks
// up: a direct child of the staging directory's tmp.
func isProbe(p string) bool {
	return path.Dir(p) == "/disk/"+fssftp.StagingDir+"/tmp"
}

// failingPut fails the probe's create with errProbe.
type failingPut struct{ next sftp.FileWriter }

func (f failingPut) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	if isProbe(r.Filepath) {
		return nil, errProbe
	}
	return f.next.Filewrite(r)
}

// unclosable is a probe file whose close fails, as a full disk or a quota
// would make it.
type unclosable struct{ io.WriterAt }

func (unclosable) Close() error { return errProbe }

// failingClose creates the probe file but fails its close.
type failingClose struct{ next sftp.FileWriter }

func (f failingClose) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	w, err := f.next.Filewrite(r)
	if err != nil || !isProbe(r.Filepath) {
		return w, err
	}
	return unclosable{w}, nil
}

// failingLstat fails the probe's look-up of the upper-case name. It does
// not implement sftp.LstatFileLister, so the server hands the probe's
// Lstat to Filelist as a "Stat".
type failingLstat struct{ next sftp.FileLister }

func (f failingLstat) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	if (r.Method == "Stat" || r.Method == "Lstat") && isProbe(r.Filepath) && path.Base(r.Filepath) == strings.ToUpper(path.Base(r.Filepath)) {
		return nil, errProbe
	}
	return f.next.Filelist(r)
}

// countingCloser records how often the driver closed a connection, and
// closes it server first: the client's Close waits for its reader, which
// ends only when the server side of the pipe is closed.
type countingCloser struct {
	srv    *sftp.RequestServer
	c      *sftp.Client
	closed *atomic.Int32
}

func (c countingCloser) Close() error {
	c.closed.Add(1)
	_ = c.srv.Close()
	return c.c.Close()
}

// TestNewProbeFailures proves a case probe that fails for any reason
// other than a read-only account (the create, the close, the look-up of
// the other case) fails New with that error rather than guessing the
// filesystem's case behaviour, and that New closes the connection it
// dialled itself.
func TestNewProbeFailures(t *testing.T) {
	for name, fault := range map[string]func(*sftp.Handlers){
		"create": func(h *sftp.Handlers) { h.FilePut = failingPut{next: h.FilePut} },
		"close":  func(h *sftp.Handlers) { h.FilePut = failingClose{next: h.FilePut} },
		"lookup": func(h *sftp.Handlers) { h.FileList = failingLstat{next: h.FileList} },
	} {
		t.Run(name, func(t *testing.T) {
			h := sftp.InMemHandler()
			fault(&h)
			var closed atomic.Int32
			dial := func(context.Context) (*sftp.Client, io.Closer, error) {
				c2sR, c2sW := io.Pipe()
				s2cR, s2cW := io.Pipe()
				srv := sftp.NewRequestServer(struct {
					io.Reader
					io.WriteCloser
				}{c2sR, s2cW}, h)
				go func() { _ = srv.Serve() }()
				client, err := sftp.NewClientPipe(s2cR, c2sW)
				if err != nil {
					return nil, nil, err
				}
				t.Cleanup(func() { _ = c2sR.Close() })
				return client, countingCloser{srv: srv, c: client, closed: &closed}, nil
			}
			drv, err := fssftp.New(fssftp.Config{Dial: dial, Root: "/disk"})
			if drv != nil || err == nil || !strings.Contains(err.Error(), errProbe.Error()) {
				t.Fatalf("New = %v, %v; want the probe's failure", drv, err)
			}
			if closed.Load() != 1 {
				t.Errorf("New closed its dialled connection %d times, want 1", closed.Load())
			}
		})
	}
}
