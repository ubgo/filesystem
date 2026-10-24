package s3_test

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
	"github.com/ubgo/filesystem/layer"
)

// TestInterceptorsKeepEveryCapability is the spike ADR-0003 asked for: the
// S3 driver wrapped in five interceptors keeps presigning, multipart and the
// As escape hatch, because interceptors wrap operations, not the driver.
func TestInterceptorsKeepEveryCapability(t *testing.T) {
	ctx := t.Context()
	client, bucket := fakeS3(t)
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	var observed []filesystem.OpKind
	d, err := filesystem.NewDisk("s3", drv, filesystem.WithInterceptors(
		layer.Observe(func(_ context.Context, e layer.Event) { observed = append(observed, e.Op.Kind) }),
		layer.Log(slog.New(slog.NewTextHandler(io.Discard, nil))),
		layer.Retry(layer.RetryOptions{}),
		layer.Timeout(5*time.Second),
		layer.Limit(8),
	))
	if err != nil {
		t.Fatal(err)
	}
	if d.Capabilities() != drv.Capabilities() {
		t.Fatalf("interceptors changed capabilities: %v vs %v", d.Capabilities(), drv.Capabilities())
	}
	if c, ok := filesystem.As[*s3.Client](d); !ok || c != client {
		t.Fatal("As lost the native client behind interceptors")
	}
	req, err := d.SignedURL(ctx, "k", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	if err != nil || !strings.Contains(req.URL, "X-Amz-Signature") {
		t.Fatalf("presign behind interceptors = %v", err)
	}
	id, err := d.CreateMultipart(ctx, "big", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	part, err := d.UploadPart(ctx, "big", id, 1, strings.NewReader("part"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.PresignPart(ctx, "big", id, 2, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CompleteMultipart(ctx, "big", id, []filesystem.Part{part}); err != nil {
		t.Fatal(err)
	}
	want := []filesystem.OpKind{filesystem.OpPresign, filesystem.OpMultipartCreate, filesystem.OpMultipartUpload, filesystem.OpMultipartPresign, filesystem.OpMultipartComplete}
	if !slices.Equal(observed, want) {
		t.Errorf("observed %v, want %v", observed, want)
	}
}
