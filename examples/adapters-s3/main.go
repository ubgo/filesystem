// Command adapters-s3 walks through the S3 driver: building the client,
// opening a disk from a Config and from a DSN, stored headers and metadata,
// signed downloads and uploads, multipart uploads with presigned parts,
// batch deletes, the capabilities the driver refuses to fake, and (in
// features.go) the object-store features the fake can model or record.
//
// It runs against an in-process fake S3 (gofakes3), so it needs no account
// and no network. Against a real provider only the client's endpoint,
// region and credentials change.
//
//	go run ./adapters-s3            (from the examples directory)
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// Values of the fake S3 this program runs against. A real deployment takes
// the region and endpoint from its configuration and the keys from the
// environment or an instance role.
const (
	// bucketName is the bucket every disk in this program uses.
	bucketName = "media"
	// fakeRegion is any valid region: the fake ignores it, but SigV4
	// signing needs one.
	fakeRegion = "us-east-1"
	// fakeAccessKey and fakeSecretKey are accepted by the fake as any value.
	fakeAccessKey = "test"
	fakeSecretKey = "test"
)

// partSize is the multipart part size this program configures: 5 MiB, the
// smallest part S3 accepts for every part but the last.
const partSize = 5 << 20

// linkLifetime is how long the signed URLs in this program stay valid.
const linkLifetime = 5 * time.Minute

func main() {
	srv, fake := startFakeS3()
	defer srv.Close()
	// The opener reads credentials from the AWS default chain; the fake
	// accepts any, so this program provides them the way a deployment does.
	_ = os.Setenv("AWS_ACCESS_KEY_ID", fakeAccessKey)
	_ = os.Setenv("AWS_SECRET_ACCESS_KEY", fakeSecretKey)
	if err := run(context.Background(), os.Stdout, fake); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// startFakeS3 serves an in-memory S3 API (see fakeS3) on a local port.
func startFakeS3() (*httptest.Server, *fakeS3) {
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	fake.endpoint = srv.URL
	return srv, fake
}

// run is main without the process: every step prints what it saw to out.
func run(ctx context.Context, out io.Writer, fake *fakeS3) error {
	endpoint := fake.endpoint
	client := newClient(endpoint)
	if err := createBucket(ctx, client); err != nil {
		return err
	}
	disk, err := openWithConfig(client)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "capabilities: %s\n", disk.Capabilities())
	steps := []func(context.Context, io.Writer, *filesystem.Disk) error{
		writeWithMetadata, signedDownload, signedUpload, presignedParts, deleteMany, refusedOptions,
	}
	for _, step := range steps {
		if err := step(ctx, out, disk); err != nil {
			return err
		}
	}
	viaDSN, err := openWithDSN(ctx, endpoint)
	if err != nil {
		return err
	}
	e, err := viaDSN.Stat(ctx, "reports/q3.csv")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "via DSN: %s is %d bytes\n", e.Key, e.Size)
	return runFeatures(ctx, out, client, fake)
}

// newClient builds the S3 client the way an application does: region,
// endpoint, credentials and addressing style are the client's business,
// not the driver's. Path-style addressing (endpoint/bucket/key) is what
// most self-hosted stores need.
// ds:def id=newclient-gwqpka5e
func newClient(endpoint string) *s3.Client {
	return s3.New(s3.Options{
		Region:       fakeRegion,
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(fakeAccessKey, fakeSecretKey, ""),
	})
}

// createBucket creates the bucket on the fresh fake. Against a real
// provider the bucket is created once, outside the application.
func createBucket(ctx context.Context, client *s3.Client) error {
	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucketName)})
	return err
}

// openWithConfig builds the driver from a Config and wraps it in a Disk.
// Visibility, ConditionalWrites and PruneEmptyPrefixes are left at their
// defaults, which are correct on every S3-compatible provider.
// ds:def id=openwithconfig-3yavfaxb
func openWithConfig(client *s3.Client) (*filesystem.Disk, error) {
	drv, err := fss3.New(fss3.Config{
		Client:   client,
		Bucket:   bucketName,
		PartSize: partSize,
	})
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk("media", drv)
}

// mediaDSN is the DSN of the same bucket. Nothing secret is in it: the
// credentials come from the AWS default chain.
// ds:def id=mediadsn-6tm5jvuy
func mediaDSN(endpoint string) string {
	q := url.Values{}
	q.Set(fss3.ParamRegion, fakeRegion)
	q.Set(fss3.ParamEndpoint, endpoint)
	q.Set(fss3.ParamPathStyle, "true")
	q.Set(fss3.ParamPartSize, fmt.Sprint(partSize))
	return fss3.Scheme + "://" + bucketName + "?" + q.Encode()
}

// openWithDSN opens the bucket through a Mux, the way configuration read
// from the environment does.
// ds:def id=openwithdsn-wrexy9vp
func openWithDSN(ctx context.Context, endpoint string) (*filesystem.Disk, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
		return nil, err
	}
	return mux.Open(ctx, "media-dsn", mediaDSN(endpoint))
}

// writeWithMetadata stores an object with a content type, user metadata
// and a Content-Disposition header, then reads them back with Stat.
// ds:def id=writewithmetadata-83sa5aek
func writeWithMetadata(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	_, err := disk.WriteBytes(ctx, "reports/q3.csv", []byte("month,total\njuly,42\n"), filesystem.WriteOptions{
		ContentType: "text/csv",
		Metadata:    map[string]string{"owner": "finance"},
		Headers:     filesystem.HTTPHeaders{ContentDisposition: `attachment; filename="q3.csv"`},
	})
	if err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "reports/q3.csv")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stat: %s %s owner=%s disposition=%s\n", e.Key, e.ContentType, e.Metadata["owner"], e.Headers.ContentDisposition)
	return nil
}

// signedDownload issues a SigV4 GET URL that forces a download, and
// fetches it with a plain HTTP client, as a browser would.
// ds:def id=signeddownload-tqzmbcpn
func signedDownload(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	req, err := disk.SignedURL(ctx, "reports/q3.csv", filesystem.PresignOptions{
		Method:             filesystem.MethodGet,
		Expires:            linkLifetime,
		ContentDisposition: "attachment",
	})
	if err != nil {
		return err
	}
	status, body, err := send(ctx, http.MethodGet, req.URL, nil, req.Header)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signed GET: %d %q\n", status, firstLine(body))
	return nil
}

// signedUpload issues a SigV4 PUT URL. The content type is part of the
// signature, so the client must send exactly the headers in req.Header.
// ds:def id=signedupload-k55hrh6n
func signedUpload(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	req, err := disk.SignedURL(ctx, "inbox/note.txt", filesystem.PresignOptions{
		Method:      filesystem.MethodPut,
		Expires:     linkLifetime,
		ContentType: "text/plain",
	})
	if err != nil {
		return err
	}
	status, _, err := send(ctx, http.MethodPut, req.URL, strings.NewReader("sent by the browser"), req.Header)
	if err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "inbox/note.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signed PUT: %d, stored %d bytes as %s\n", status, e.Size, e.ContentType)
	return nil
}

// presignedParts runs a multipart upload whose part goes straight from the
// client to the bucket: the server creates the upload, signs each part and
// completes it with the parts S3 recorded.
// ds:def id=presignedparts-6m8rgtgq
func presignedParts(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	const key = "videos/clip.bin"
	id, err := disk.CreateMultipart(ctx, key, filesystem.WriteOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return err
	}
	part, err := disk.PresignPart(ctx, key, id, 1, linkLifetime)
	if err != nil {
		return err
	}
	if _, _, err := send(ctx, http.MethodPut, part.URL, strings.NewReader("one part, sent directly"), part.Header); err != nil {
		return err
	}
	parts, err := disk.ListParts(ctx, key, id)
	if err != nil {
		return err
	}
	e, err := disk.CompleteMultipart(ctx, key, id, parts)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "multipart: %d part(s), %s is %d bytes\n", len(parts), e.Key, e.Size)
	return nil
}

// deleteMany deletes a list of keys (1,000 per DeleteObjects request) and
// then everything under a prefix.
// ds:def id=deletemany-9kakcjsx
func deleteMany(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	keys := []string{"tmp/a.txt", "tmp/b.txt", "tmp/nested/c.txt"}
	for _, k := range keys {
		if _, err := disk.WriteBytes(ctx, k, []byte("x"), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	if err := disk.DeleteKeys(ctx, keys[:2]); err != nil {
		return err
	}
	if err := disk.DeletePrefix(ctx, "tmp"); err != nil {
		return err
	}
	page, err := disk.ListPage(ctx, "tmp", filesystem.ListOptions{Recursive: true})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "after deletes: %d keys under tmp/\n", len(page.Entries))
	return nil
}

// refusedOptions shows the default configuration refusing what it cannot
// guarantee: a per-object public file (no ACLs declared) and a conditional
// write (If-None-Match not declared). Both are CodeUnsupported, never a
// silent overwrite or a silently private file.
// ds:def id=refusedoptions-5dwdvy3h
func refusedOptions(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	_, err := disk.WriteBytes(ctx, "public.txt", []byte("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic})
	fmt.Fprintf(out, "public write: %s\n", filesystem.CodeOf(err))
	_, err = disk.WriteBytes(ctx, "once.txt", []byte("x"), filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "conditional write: %s\n", filesystem.CodeOf(err))
	return nil
}

// send performs one HTTP request with the given headers and returns the
// status and body.
func send(ctx context.Context, method, rawURL string, body io.Reader, h http.Header) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return 0, "", err
	}
	for k, v := range h {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

// firstLine returns s up to its first newline.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
