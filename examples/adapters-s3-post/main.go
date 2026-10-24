// Command adapters-s3-post walks through a browser form upload with a
// presigned POST: the server signs a policy (size range, key prefix,
// content type, stored headers, expiry), renders the HTML form a browser
// submits straight to the bucket, and confirms the stored object.
//
// It runs against an in-process fake S3 (gofakes3), which stores form
// uploads but does not check the policy; the driver's real-store tests
// show SeaweedFS and RustFS refusing uploads outside it.
//
//	go run ./adapters-s3-post            (from the examples directory)
package main

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
	"github.com/ubgo/filesystem/upload"
)

// Values of the fake S3 this program runs against.
const (
	bucketName    = "media"
	fakeRegion    = "us-east-1"
	fakeAccessKey = "test"
	fakeSecretKey = "test"
)

// Limits of the avatar form: what the policy lets a browser upload.
const (
	avatarMaxSize  = 2 << 20 // 2 MiB
	avatarType     = "image/png"
	formLifetime   = 10 * time.Minute
	avatarCacheFor = "public, max-age=86400"
)

// pngBytes is a 1×1 PNG, the "file" the simulated browser uploads.
var pngBytes = []byte{
	0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0x0d, 'I', 'H', 'D', 'R',
	0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89,
	0, 0, 0, 0x0a, 'I', 'D', 'A', 'T', 0x78, 0x9c, 0x63, 0, 1, 0, 0, 5, 0, 1, 0x0d, 0x0a, 0x2d, 0xb4,
	0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
}

func main() {
	srv := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
	defer srv.Close()
	if err := run(context.Background(), os.Stdout, srv.URL); err != nil {
		fmt.Fprintln(os.Stderr, err)
		srv.Close()
		os.Exit(1)
	}
}

// run is main without the process: every step prints what it saw to out.
func run(ctx context.Context, out io.Writer, endpoint string) error {
	disk, err := openDisk(ctx, endpoint)
	if err != nil {
		return err
	}
	post, err := signAvatarForm(ctx, disk, "users/42")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "form posts to %s\n", post.URL)
	fmt.Fprintf(out, "key field: %s\n", post.Fields[filesystem.PostFieldKey])
	if err := renderForm(out, post); err != nil {
		return err
	}
	// What the browser does when the user submits the form: every field,
	// then the file, last. This program chooses a key under the prefix.
	key := "users/42/avatar.png"
	status, err := submit(ctx, post, key, "avatar.png", pngBytes)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "browser upload: %d\n", status)
	res, err := confirm(ctx, disk, key)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "confirmed: %s %d bytes %s\n", res.Key, res.Size, res.ContentType)
	return nil
}

// openDisk builds the S3 client and the disk, as an application would.
func openDisk(ctx context.Context, endpoint string) (*filesystem.Disk, error) {
	client := s3.New(s3.Options{
		Region:       fakeRegion,
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(fakeAccessKey, fakeSecretKey, ""),
	})
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucketName)}); err != nil {
		return nil, err
	}
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: bucketName})
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk("media", drv)
}

// signAvatarForm signs a form that lets a browser upload one PNG of at
// most 2 MiB under dir, stored with a Cache-Control header, for ten
// minutes. S3 refuses anything else.
// ds:def id=signavatarform-trkzpzgm
func signAvatarForm(ctx context.Context, disk *filesystem.Disk, dir string) (filesystem.PresignedPost, error) {
	return disk.PresignPost(ctx, filesystem.PostPolicy{
		KeyPrefix:   dir,
		ContentType: avatarType,
		MinSize:     1,
		MaxSize:     avatarMaxSize,
		Expires:     formLifetime,
		Headers:     filesystem.HTTPHeaders{CacheControl: avatarCacheFor},
	})
}

// formTemplate is the HTML a page serves: one hidden input per field, then
// the file input, which must come last.
var formTemplate = template.Must(template.New("form").Parse(`<form action="{{.URL}}" method="post" enctype="multipart/form-data">
{{- range .Fields}}
  <input type="hidden" name="{{.Name}}" value="{{.Value}}">
{{- end}}
  <input type="file" name="{{.File}}" accept="{{.Accept}}">
  <button>Upload</button>
</form>
`))

// formField is one hidden input of the form.
type formField struct{ Name, Value string }

// renderForm prints the HTML form for post, fields in a stable order.
// ds:def id=renderform-63j5hz9w
func renderForm(out io.Writer, post filesystem.PresignedPost) error {
	fields := make([]formField, 0, len(post.Fields))
	for name, value := range post.Fields {
		if name == "policy" || name == "X-Amz-Signature" {
			value = value[:8] + "…" // shortened for the printout only
		}
		fields = append(fields, formField{name, value})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
	return formTemplate.Execute(out, struct {
		URL    string
		Fields []formField
		File   string
		Accept string
	}{post.URL, fields, filesystem.PostFileField, avatarType})
}

// submit posts the form the way a browser does: every field (with key set
// to a key under the prefix), then the file.
// ds:def id=submit-we8jrp97
func submit(ctx context.Context, post filesystem.PresignedPost, key, filename string, file []byte) (int, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for name, value := range post.Fields {
		if name == filesystem.PostFieldKey {
			value = key
		}
		if err := w.WriteField(name, value); err != nil {
			return 0, err
		}
	}
	fw, err := w.CreateFormFile(filesystem.PostFileField, filename)
	if err != nil {
		return 0, err
	}
	if _, err := fw.Write(file); err != nil {
		return 0, err
	}
	if err := w.Close(); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, post.URL, &body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// confirm checks the stored object against the same limits from its
// bytes, as every direct upload must be before the application trusts it.
func confirm(ctx context.Context, disk *filesystem.Disk, key string) (upload.Result, error) {
	return upload.Confirm(ctx, disk, key, "avatar.png", upload.Policy{MaxSize: avatarMaxSize, Allowed: []string{avatarType}})
}
