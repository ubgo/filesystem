package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
)

// Direct uploads in this demo go to a memory disk whose links are signed
// by signedurl and served by fshttp, which behaves like an S3 bucket.
const (
	// signedPrefix is where fshttp.Signed is mounted.
	signedPrefix = "/files"
	// ticketLifetime is how long the client has to start its upload.
	ticketLifetime = 10 * time.Minute
)

// demoSecret is a fixed signing secret so the demo is repeatable; a real
// one comes from a secret store.
var demoSecret = bytes.Repeat([]byte("k"), signedurl.MinKeyBytes)

// bucket is a disk that accepts signed PUTs, and the server answering them.
type bucket struct {
	disk *filesystem.Disk
	srv  *httptest.Server
}

// newBucket wires a memory disk, a signer and fshttp.Signed.
func newBucket() (*bucket, error) {
	routes := http.NewServeMux()
	srv := httptest.NewServer(routes)
	signer, err := signedurl.New(srv.URL+signedPrefix, []signedurl.Key{{ID: "k1", Secret: demoSecret}})
	if err != nil {
		srv.Close()
		return nil, err
	}
	disk, err := filesystem.NewDisk("direct", memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		srv.Close()
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		srv.Close()
		return nil, err
	}
	routes.Handle(signedPrefix+"/", http.StripPrefix(signedPrefix, fshttp.Signed(disks, signer)))
	return &bucket{disk: disk, srv: srv}, nil
}

// put sends body the way a browser follows a Ticket.
func put(ctx context.Context, t upload.Ticket, body string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, t.Method, t.URL, bytes.NewReader([]byte(body)))
	if err != nil {
		return 0, err
	}
	for k, v := range t.Headers {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// direct runs the two-step flow: Presign checks what the client declares
// and returns a Ticket; the client uploads straight to the bucket; Confirm
// then checks the stored bytes. A client that declared a PNG but sent
// HTML passes the first step and is caught, and deleted, by the second.
// ds:def id=direct-gtjfeh86
func direct(ctx context.Context, out io.Writer) error {
	b, err := newBucket()
	if err != nil {
		return err
	}
	defer b.srv.Close()
	for _, body := range []string{pngFile, htmlFile} {
		req := upload.TicketRequest{Filename: "photo.png", ContentType: "image/png", Size: int64(len(body))}
		t, err := upload.Presign(ctx, b.disk, req, policy, ticketLifetime)
		if err != nil {
			return err
		}
		status, err := put(ctx, t, body)
		if err != nil {
			return err
		}
		res, err := upload.Confirm(ctx, b.disk, t.Key, req.Filename, policy)
		left, _ := b.disk.Exists(ctx, t.Key)
		fmt.Fprintf(out, "direct: PUT %d, confirm %q type %q, object kept: %t\n", status, filesystem.CodeOf(err), res.ContentType, left)
	}
	_, err = upload.Presign(ctx, b.disk, upload.TicketRequest{Filename: "page.html", ContentType: "text/html", Size: 1}, policy, ticketLifetime)
	fmt.Fprintf(out, "presign declared text/html: %s\n", filesystem.CodeOf(err))
	return nil
}

// uppy shows the Uppy endpoints refusing a disk that cannot sign part
// uploads: they need the Multipart and PresignPart capabilities, which
// only the S3 driver has.
// ds:def id=uppy-gknjx429
func uppy(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	h := upload.Uppy(disk, policy, upload.UppyOptions{
		Authorize: func(*http.Request, string) error { return nil }, // check the key belongs to the user
	})
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/multipart", bytes.NewReader([]byte(`{"filename":"a.png","type":"image/png"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	fmt.Fprintf(out, "uppy on memory: %d %s", rec.Code, rec.Body.String())
	return nil
}
