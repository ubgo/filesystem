package s3_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
	"github.com/ubgo/filesystem/filesystemtest"
)

// TestConformanceSweepUploads runs the suite with the opt-in sweep, so its
// SweepAbandoned case (an upload in progress survives an old cutoff and is
// aborted by a new one, stored objects untouched) runs against the fake.
func TestConformanceSweepUploads(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		d, _ := newDisk(t, fss3.Config{SweepUploads: true})
		return d
	})
}

// TestSweepUploads proves the sweep aborts uploads initiated before the
// cutoff, keeps newer ones, and is declared only when asked for.
func TestSweepUploads(t *testing.T) {
	ctx := t.Context()
	if off, _ := newDisk(t, fss3.Config{}); off.Capabilities().SweepAbandoned {
		t.Error("SweepAbandoned declared without SweepUploads")
	}
	d, _ := newDisk(t, fss3.Config{SweepUploads: true})
	a, _ := d.CreateMultipart(ctx, "a.bin", filesystem.WriteOptions{})
	b, _ := d.CreateMultipart(ctx, "dir/b.bin", filesystem.WriteOptions{})
	if n, err := d.SweepAbandoned(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("sweep with an old cutoff = %d, %v", n, err)
	}
	n, err := d.SweepAbandoned(ctx, time.Now().Add(time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("sweep = %d, %v; want 2", n, err)
	}
	for key, id := range map[string]string{"a.bin": a, "dir/b.bin": b} {
		if _, err := d.ListParts(ctx, key, id); filesystem.CodeOf(err) != filesystem.CodeNotFound {
			t.Errorf("%s after sweep = %v", key, err)
		}
	}
}

// TestSweepUploadsFailures covers a failed listing, a failed abort, and
// an upload that vanished between the listing and the abort.
func TestSweepUploadsFailures(t *testing.T) {
	ctx := t.Context()
	later := time.Now().Add(time.Hour)
	for _, c := range []struct {
		name          string
		method, match string
		status        int
		code          string
		wantErr       bool
	}{
		{"list", http.MethodGet, "uploads", http.StatusInternalServerError, "InternalError", true},
		{"abort", http.MethodDelete, "uploadId", http.StatusInternalServerError, "InternalError", true},
		{"vanished", http.MethodDelete, "uploadId", http.StatusNotFound, "NoSuchUpload", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, p := newDisk(t, fss3.Config{SweepUploads: true})
			if _, err := d.CreateMultipart(ctx, "x.bin", filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			p.fail(c.method, c.match, c.status, c.code)
			n, err := d.SweepAbandoned(ctx, later)
			if (err != nil) != c.wantErr || n != 0 {
				t.Errorf("sweep = %d, %v; want 0, error %t", n, err, c.wantErr)
			}
		})
	}
}

// TestSweepUploadsFromDSN proves sweep_uploads=true declares the sweep.
func TestSweepUploadsFromDSN(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")
	mux := filesystem.NewMux()
	_ = mux.Register(fss3.Scheme, fss3.Opener)
	d, err := mux.Open(t.Context(), "s3", "s3://b?region=us-east-1&sweep_uploads=true")
	if err != nil || !d.Capabilities().SweepAbandoned || !strings.Contains(d.Capabilities().String(), "sweep-abandoned") {
		t.Fatalf("sweep_uploads=true = %v, %v", d, err)
	}
}
