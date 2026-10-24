// Command concepts-upload-clients shows the upload package from the
// client's side: the JSON every upload endpoint answers with, chunked
// uploads as Dropzone, FilePond and resumable.js send them, progress
// reported while a file streams, a dropped folder keeping its layout, and
// retries made safe by an Idempotency-Key.
//
//	go run ./concepts-upload-clients          (from the examples directory)
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// Sample content whose first bytes sniff recognises as a PNG, padded so
// the chunked and progress steps have something to split.
const pngHeader = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"

// photo is the file most steps send: 300 bytes that detect as image/png.
var photo = pngHeader + strings.Repeat("p", 300-len(pngHeader))

// Limits and names of the demo.
const (
	// maxUpload is the policy's per-file limit: 1 MiB.
	maxUpload = 1 << 20
	// chunkSize is how the chunked client splits photo: three chunks.
	chunkSize = 100
	// staging is where chunks wait until the file is complete.
	staging = "staging"
	// demoUser stands in for the signed-in user every request comes from.
	demoUser = "u1"
)

// policy accepts images up to maxUpload under random keys.
var policy = upload.Policy{
	MaxSize: maxUpload,
	MinSize: 1,
	Allowed: []string{"image/*"},
	Key:     upload.RandomKey("uploads"),
}

// owner names who sent a request. A real app reads the signed-in user
// from the session; one user is enough here.
// ds:def id=owner-nkvbk4pn
func owner(*http.Request) (string, error) { return demoUser, nil }

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is main without the process, so the test can call it.
func run(ctx context.Context, out io.Writer) error {
	for _, step := range []func(context.Context, io.Writer) error{
		jsonContract, chunked, progress, folder, idempotent,
	} {
		if err := step(ctx, out); err != nil {
			return err
		}
	}
	return nil
}

// newDisk returns an empty memory disk for one step.
func newDisk() (*filesystem.Disk, error) {
	return filesystem.NewDisk("uploads", memory.New())
}

// part is one multipart form part: a plain field when file is empty,
// otherwise a file with that client-side name.
type part struct{ field, file, value string }

// post sends parts to h as one multipart/form-data POST, with extra
// headers, and returns the recorded response.
// ds:def id=post-2ttqjbqb
func post(ctx context.Context, h http.Handler, header http.Header, parts ...part) (*httptest.ResponseRecorder, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		var w io.Writer
		var err error
		if p.file == "" {
			w, err = mw.CreateFormField(p.field)
		} else {
			w, err = mw.CreateFormFile(p.field, p.file)
		}
		if err != nil {
			return nil, err
		}
		if _, err := io.WriteString(w, p.value); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, nil
}

// file is a part holding one file in the field the handlers read.
func file(name, data string) part { return part{field: upload.DefaultField, file: name, value: data} }

// itoa formats a form field's number.
func itoa(n int) string { return strconv.Itoa(n) }
