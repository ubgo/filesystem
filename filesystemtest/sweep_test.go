package filesystemtest_test

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/filesystemtest"
	"github.com/ubgo/filesystem/memory"
)

// errInjected is the failure the sweep injects. Its code, CodePermission,
// is one no case expects, so a case that reports success after it fired
// has swallowed an error.
var errInjected = filesystem.NewError(filesystem.CodePermission, errors.New("injected fault"))

// tolerated lists cases that may legitimately pass after an injected fault,
// because the faulted call is one where the case expects some failure.
var tolerated = map[string]bool{
	// Its write must fail; a fault on that write is also a failure.
	"FailingBodyStoresNothing": true,
	// One of its appends must fail; a fault on that append is a failure too.
	"Append": true,
}

// TestCasesReportEveryFailure fails the 1st, 2nd, 3rd… driver call of every
// case in turn and requires the case to report it. This proves the suite
// propagates errors from every step instead of passing vacuously when a
// driver call fails.
func TestCasesReportEveryFailure(t *testing.T) {
	for _, c := range filesystemtest.Cases() {
		t.Run(c.Name, func(t *testing.T) {
			for k := int64(1); ; k++ {
				drv := &failNth{Driver: memory.New(), n: k}
				d, err := filesystem.NewDisk("sweep", drv)
				if err != nil {
					t.Fatal(err)
				}
				if c.Needs != nil && !c.Needs(d.Capabilities()) {
					// Cases for drivers without multipart uploads run on
					// the memory driver with Multipart hidden.
					drv.noMultipart = true
					if d, err = filesystem.NewDisk("sweep", drv); err != nil {
						t.Fatal(err)
					}
				}
				if c.Needs != nil && !c.Needs(d.Capabilities()) {
					t.Skip("not applicable to the memory driver")
				}
				err = c.Check(t.Context(), d)
				if !drv.fired.Load() {
					if err != nil {
						t.Fatalf("case failed without a fault: %v", err)
					}
					return
				}
				if err == nil && !tolerated[c.Name] {
					t.Fatalf("case passed although driver call %d failed", k)
				}
			}
		})
	}
}

// failNth is the memory driver failing exactly its n-th call, optionally
// without its Multipart capability.
type failNth struct {
	*memory.Driver
	n           int64
	calls       atomic.Int64
	fired       atomic.Bool
	noMultipart bool
}

func (f *failNth) Capabilities() filesystem.Capabilities {
	c := f.Driver.Capabilities()
	if f.noMultipart {
		c.Multipart = false
	}
	return c
}

func (f *failNth) fail() bool {
	if f.calls.Add(1) == f.n {
		f.fired.Store(true)
		return true
	}
	return false
}

func (f *failNth) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	if f.fail() {
		return filesystem.Entry{}, errInjected
	}
	return f.Driver.Stat(ctx, key)
}

func (f *failNth) Read(ctx context.Context, key string, r filesystem.ByteRange) (io.ReadCloser, error) {
	if f.fail() {
		return nil, errInjected
	}
	return f.Driver.Read(ctx, key, r)
}

func (f *failNth) Write(ctx context.Context, key string, body io.Reader, o filesystem.WriteOptions) (filesystem.Entry, error) {
	if f.fail() {
		return filesystem.Entry{}, errInjected
	}
	return f.Driver.Write(ctx, key, body, o)
}

func (f *failNth) Delete(ctx context.Context, key string) error {
	if f.fail() {
		return errInjected
	}
	return f.Driver.Delete(ctx, key)
}

func (f *failNth) List(ctx context.Context, dir string, o filesystem.ListOptions) (filesystem.Page, error) {
	if f.fail() {
		return filesystem.Page{}, errInjected
	}
	return f.Driver.List(ctx, dir, o)
}

func (f *failNth) Copy(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	if f.fail() {
		return filesystem.Entry{}, errInjected
	}
	return f.Driver.Copy(ctx, src, dst)
}

func (f *failNth) Move(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	if f.fail() {
		return filesystem.Entry{}, errInjected
	}
	return f.Driver.Move(ctx, src, dst)
}

func (f *failNth) SetVisibility(ctx context.Context, key string, v filesystem.Visibility) error {
	if f.fail() {
		return errInjected
	}
	return f.Driver.SetVisibility(ctx, key, v)
}

func (f *failNth) DeletePrefix(ctx context.Context, dir string) error {
	if f.fail() {
		return errInjected
	}
	return f.Driver.DeletePrefix(ctx, dir)
}

func (f *failNth) DeleteKeys(ctx context.Context, keys []string) error {
	if f.fail() {
		return errInjected
	}
	return f.Driver.DeleteKeys(ctx, keys)
}

func (f *failNth) CreateMultipart(ctx context.Context, key string, o filesystem.WriteOptions) (string, error) {
	if f.fail() {
		return "", errInjected
	}
	return f.Driver.CreateMultipart(ctx, key, o)
}

func (f *failNth) UploadPart(ctx context.Context, key, id string, n int, body io.Reader, size int64) (filesystem.Part, error) {
	if f.fail() {
		return filesystem.Part{}, errInjected
	}
	return f.Driver.UploadPart(ctx, key, id, n, body, size)
}

func (f *failNth) ListParts(ctx context.Context, key, id string) ([]filesystem.Part, error) {
	if f.fail() {
		return nil, errInjected
	}
	return f.Driver.ListParts(ctx, key, id)
}

func (f *failNth) CompleteMultipart(ctx context.Context, key, id string, parts []filesystem.Part) (filesystem.Entry, error) {
	if f.fail() {
		return filesystem.Entry{}, errInjected
	}
	return f.Driver.CompleteMultipart(ctx, key, id, parts)
}

func (f *failNth) AbortMultipart(ctx context.Context, key, id string) error {
	if f.fail() {
		return errInjected
	}
	return f.Driver.AbortMultipart(ctx, key, id)
}

func (f *failNth) Append(ctx context.Context, key string, body io.Reader, o filesystem.AppendOptions) (filesystem.Entry, error) {
	if f.fail() {
		return filesystem.Entry{}, errInjected
	}
	return f.Driver.Append(ctx, key, body, o)
}

func (f *failNth) SetModTime(ctx context.Context, key string, t time.Time) error {
	if f.fail() {
		return errInjected
	}
	return f.Driver.SetModTime(ctx, key, t)
}

func (f *failNth) Tags(ctx context.Context, key string) (map[string]string, error) {
	if f.fail() {
		return nil, errInjected
	}
	return f.Driver.Tags(ctx, key)
}

func (f *failNth) SetTags(ctx context.Context, key string, tags map[string]string) error {
	if f.fail() {
		return errInjected
	}
	return f.Driver.SetTags(ctx, key, tags)
}

func (f *failNth) SweepAbandoned(ctx context.Context, before time.Time) (int, error) {
	if f.fail() {
		return 0, errInjected
	}
	return f.Driver.SweepAbandoned(ctx, before)
}

// metadataless declares no metadata, so MetadataUnsupportedIsRefused runs.
type metadataless struct{ *memory.Driver }

func (m metadataless) Capabilities() filesystem.Capabilities {
	c := m.Driver.Capabilities()
	c.Metadata = false
	return c
}

func TestMetadataRefusalCase(t *testing.T) {
	d, err := filesystem.NewDisk("plain", metadataless{memory.New()})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range filesystemtest.Cases() {
		if c.Name == "MetadataUnsupportedIsRefused" {
			if err := c.Check(t.Context(), d); err != nil {
				t.Fatal(err)
			}
			full, _ := filesystem.NewDisk("full", memory.New())
			if err := c.Check(t.Context(), full); err == nil {
				t.Fatal("case passed on a disk that stores metadata")
			}
		}
	}
}

// The refusal case must fail on a disk that sweeps, or it proves nothing.
func TestSweepAbandonedRefusalCase(t *testing.T) {
	for _, c := range filesystemtest.Cases() {
		if c.Name != "SweepAbandonedUnsupportedIsRefused" {
			continue
		}
		full, err := filesystem.NewDisk("full", memory.New())
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Check(t.Context(), full); err == nil {
			t.Fatal("case passed on a disk that sweeps")
		}
		return
	}
	t.Fatal("case SweepAbandonedUnsupportedIsRefused not found")
}

// singlePut is the memory driver without multipart uploads, the shape of
// a driver (SFTP) whose only leftovers are staging files.
type singlePut struct{ *memory.Driver }

func (s singlePut) Capabilities() filesystem.Capabilities {
	c := s.Driver.Capabilities()
	c.Multipart = false
	return c
}

func TestSuitePassesOnASweeperWithoutMultipart(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		d, err := filesystem.NewDisk("single", singlePut{memory.New()})
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
}
