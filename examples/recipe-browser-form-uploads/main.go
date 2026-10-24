// Command recipe-browser-form-uploads serves the browser side of a direct
// form upload: a page whose script asks the application for a ticket
// (upload.Presign with Policy.PostForm), posts the file straight to the
// bucket with the ticket's form fields, and asks the application to
// confirm it (upload.ConfirmExpected). The bucket enforces the form's
// policy; Confirm checks the stored bytes.
//
// By default it runs against an in-process fake S3 (gofakes3), which
// stores form uploads without checking the policy. Set
// FILESYSTEM_S3_TEST_DSN (and the AWS key variables) to run it against a
// real store, which refuses the oversize upload itself.
//
//	go run ./recipe-browser-form-uploads            (from the examples directory)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/upload"
)

// EnvDSN, when set, is the DSN of a real S3-compatible bucket to run
// against instead of the in-process fake (the bucket is created if
// missing; credentials come from the AWS environment variables).
const EnvDSN = "FILESYSTEM_S3_TEST_DSN"

// Values of the in-process fake.
const (
	fakeBucket    = "media"
	fakeRegion    = "us-east-1"
	fakeAccessKey = "test"
	fakeSecretKey = "test"
)

// Limits of the avatar upload.
const (
	avatarMaxSize  = 2 << 20 // 2 MiB
	avatarType     = "image/png"
	ticketLifetime = 10 * time.Minute
)

// Routes of the application.
const (
	routePage    = "/"
	routeTicket  = "/uploads/ticket"
	routeConfirm = "/uploads/confirm"
)

// pngBytes is a 1×1 PNG, the file the simulated browser uploads.
var pngBytes = []byte{
	0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0x0d, 'I', 'H', 'D', 'R',
	0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89,
	0, 0, 0, 0x0a, 'I', 'D', 'A', 'T', 0x78, 0x9c, 0x63, 0, 1, 0, 0, 5, 0, 1, 0x0d, 0x0a, 0x2d, 0xb4,
	0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
}

func main() {
	ctx := context.Background()
	disk, stop, err := openDisk(ctx, os.Getenv(EnvDSN))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	err = run(ctx, os.Stdout, disk)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// openDisk opens the bucket named by dsn, or an in-process fake when dsn
// is empty; stop releases what it started.
func openDisk(ctx context.Context, dsn string) (*filesystem.Disk, func(), error) {
	if dsn != "" {
		mux := filesystem.NewMux()
		if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
			return nil, nil, err
		}
		disk, err := mux.Open(ctx, "media", dsn)
		if err != nil {
			return nil, nil, err
		}
		client, _ := filesystem.As[*s3.Client](disk)
		bucket := strings.TrimPrefix(strings.SplitN(dsn, "?", 2)[0], fss3.Scheme+"://")
		return disk, func() {}, ensureBucket(ctx, client, bucket)
	}
	srv := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
	client := s3.New(s3.Options{
		Region:       fakeRegion,
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(fakeAccessKey, fakeSecretKey, ""),
	})
	if err := ensureBucket(ctx, client, fakeBucket); err != nil {
		srv.Close()
		return nil, nil, err
	}
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: fakeBucket})
	if err != nil {
		srv.Close()
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk("media", drv)
	return disk, srv.Close, err
}

// ensureBucket creates bucket unless it exists.
func ensureBucket(ctx context.Context, client *s3.Client, bucket string) error {
	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if err != nil && !errors.As(err, &owned) && !errors.As(err, &exists) {
		return err
	}
	return nil
}

// avatarPolicy is what the application accepts: one PNG of at most 2 MiB,
// stored under a random name in avatars/, uploaded with a presigned POST
// form so the bucket itself refuses a file of another size or type.
// ds:def id=avatarpolicy-4w4dsk4r
func avatarPolicy() upload.Policy {
	return upload.Policy{
		MaxSize:  avatarMaxSize,
		MinSize:  1,
		Allowed:  []string{avatarType},
		Key:      upload.RandomKey("avatars"),
		PostForm: true,
	}
}

// app is the application's HTTP side: the page, the ticket and the
// confirmation.
func app(disk *filesystem.Disk) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+routePage, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = page.Execute(w, struct{ Ticket, Confirm, Accept string }{routeTicket, routeConfirm, avatarType})
	})
	mux.HandleFunc("POST "+routeTicket, func(w http.ResponseWriter, r *http.Request) { issueTicket(w, r, disk) })
	mux.HandleFunc("POST "+routeConfirm, func(w http.ResponseWriter, r *http.Request) { confirmUpload(w, r, disk) })
	return mux
}

// issueTicket answers a browser's request for a direct upload with a form
// ticket: the URL to post to and the fields to send with the file. The
// declared size becomes the only size the bucket accepts.
// ds:def id=issueticket-8tsaxjxf
func issueTicket(w http.ResponseWriter, r *http.Request, disk *filesystem.Disk) {
	var req upload.TicketRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad ticket request", http.StatusBadRequest)
		return
	}
	ticket, err := upload.Presign(r.Context(), disk, req, avatarPolicy(), ticketLifetime)
	if err != nil {
		http.Error(w, fshttp.ClientMessage(err), fshttp.StatusOf(err))
		return
	}
	writeJSON(w, ticket)
}

// confirmRequest is what the page sends once the bucket accepted the file.
type confirmRequest struct {
	Key      string `json:"key"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

// confirmUpload checks the stored object against the policy from its own
// bytes and against the size the browser declared, deleting it when
// either fails. A real application also checks that Key is one it issued
// to this user (keep the ticket in the session).
// ds:def id=confirmupload-nxz3g54m
func confirmUpload(w http.ResponseWriter, r *http.Request, disk *filesystem.Disk) {
	var req confirmRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad confirm request", http.StatusBadRequest)
		return
	}
	res, err := upload.ConfirmExpected(r.Context(), disk, req.Key, req.Filename, avatarPolicy(), upload.Expected{Size: req.Size})
	if err != nil {
		http.Error(w, fshttp.ClientMessage(err), fshttp.StatusOf(err))
		return
	}
	writeJSON(w, res)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// page is the whole browser side: ask for a ticket, post the form fields
// then the file (last) to the bucket, then ask for confirmation. The
// bucket needs CORS for POST from the page's origin (s3.SetCORS).
// ds:def id=page-vt4vb36v
var page = template.Must(template.New("page").Parse(`<!doctype html>
<input type="file" id="file" accept="{{.Accept}}">
<script>
document.getElementById("file").onchange = async (e) => {
  const file = e.target.files[0];
  const ticket = await (await fetch("{{.Ticket}}", {method: "POST",
    body: JSON.stringify({filename: file.name, content_type: file.type, size: file.size})})).json();
  const form = new FormData();
  for (const [name, value] of Object.entries(ticket.fields)) form.append(name, value);
  form.append("file", file);
  const sent = await fetch(ticket.url, {method: "POST", body: form});
  if (!sent.ok) throw new Error("upload refused: " + sent.status);
  const done = await fetch("{{.Confirm}}", {method: "POST",
    body: JSON.stringify({key: ticket.key, filename: file.name, size: file.size})});
  console.log(await done.json());
};
</script>
`))

// run starts the application and plays the browser: one avatar inside the
// policy, then one whose bytes are larger than the size it declared.
func run(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	srv := httptest.NewServer(app(disk))
	defer srv.Close()
	resp, err := http.Get(srv.URL + routePage)
	if err != nil {
		return err
	}
	html, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	fmt.Fprintf(out, "page: %d bytes, form posts to the bucket from script\n", len(html))
	b := browser{app: srv.URL}

	ticket, err := b.ticket(ctx, "me.png", avatarType, int64(len(pngBytes)))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "ticket: %s %s key=%s\n", ticket.Method, hostOf(ticket.URL), shape(ticket.Key))
	status, err := b.submit(ctx, ticket, "me.png", pngBytes)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "bucket accepted the form: %d\n", status)
	confirmed, body, err := b.confirm(ctx, ticket.Key, "me.png", int64(len(pngBytes)))
	if err != nil {
		return err
	}
	var res upload.Result
	if confirmed != http.StatusOK || json.Unmarshal([]byte(body), &res) != nil {
		return fmt.Errorf("confirm: %d %s", confirmed, body)
	}
	fmt.Fprintf(out, "confirm: %d %s %d bytes, key %s\n", confirmed, res.ContentType, res.Size, shape(res.Key))

	// The browser declares the PNG's size but sends a larger file.
	ticket, err = b.ticket(ctx, "big.png", avatarType, int64(len(pngBytes)))
	if err != nil {
		return err
	}
	oversize := append(append([]byte{}, pngBytes...), make([]byte, 1024)...)
	status, err = b.submit(ctx, ticket, "big.png", oversize)
	if err != nil {
		return err
	}
	if status >= http.StatusBadRequest {
		fmt.Fprintf(out, "oversize upload: refused by the bucket (%d)\n", status)
		return nil
	}
	fmt.Fprintf(out, "oversize upload: accepted by the store (%d), which does not check the policy\n", status)
	confirmed, body, err = b.confirm(ctx, ticket.Key, "big.png", int64(len(pngBytes)))
	if err != nil {
		return err
	}
	exists, _ := disk.Exists(ctx, ticket.Key)
	fmt.Fprintf(out, "confirm: %d %s (object kept: %t)\n", confirmed, body, exists)
	return nil
}

// browser does what the page's script does, over HTTP.
type browser struct{ app string }

func (b browser) ticket(ctx context.Context, name, contentType string, size int64) (upload.Ticket, error) {
	var t upload.Ticket
	body, _ := json.Marshal(upload.TicketRequest{Filename: name, ContentType: contentType, Size: size})
	status, resp, err := post(ctx, b.app+routeTicket, "application/json", body)
	if err != nil {
		return t, err
	}
	if status != http.StatusOK {
		return t, fmt.Errorf("ticket: %d %s", status, resp)
	}
	return t, json.Unmarshal(resp, &t)
}

// submit posts the ticket's fields, then the file, last, to the bucket.
func (b browser) submit(ctx context.Context, t upload.Ticket, name string, file []byte) (int, error) {
	var form bytes.Buffer
	w := multipart.NewWriter(&form)
	for k, v := range t.Fields {
		if err := w.WriteField(k, v); err != nil {
			return 0, err
		}
	}
	fw, err := w.CreateFormFile(filesystem.PostFileField, name)
	if err != nil {
		return 0, err
	}
	if _, err := fw.Write(file); err != nil {
		return 0, err
	}
	if err := w.Close(); err != nil {
		return 0, err
	}
	status, _, err := post(ctx, t.URL, w.FormDataContentType(), form.Bytes())
	return status, err
}

func (b browser) confirm(ctx context.Context, key, name string, size int64) (int, string, error) {
	body, _ := json.Marshal(confirmRequest{Key: key, Filename: name, Size: size})
	status, resp, err := post(ctx, b.app+routeConfirm, "application/json", body)
	return status, strings.TrimSpace(string(resp)), err
}

func post(ctx context.Context, url, contentType string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

// hostOf hides the random port of the fake in the printout.
func hostOf(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.IndexAny(u, ":/"); i >= 0 {
		return u[:i]
	}
	return u
}

// shape prints a random key with its random part elided.
func shape(key string) string {
	dir, name, _ := strings.Cut(key, "/")
	ext := name[strings.LastIndex(name, "."):]
	return dir + "/<random>" + ext
}
