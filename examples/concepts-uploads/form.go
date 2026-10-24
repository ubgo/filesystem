package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/ubgo/filesystem/upload"
)

// maxFilesPerRequest caps how many files one form post may carry.
const maxFilesPerRequest = 3

// formFile is one file of a multipart form.
type formFile struct{ name, data string }

// postForm sends files to h as a multipart/form-data POST in the field
// Handler reads, and returns the status and the JSON body.
func postForm(ctx context.Context, h http.Handler, files ...formFile) (int, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range files {
		w, err := mw.CreateFormFile(upload.DefaultField, f.name)
		if err != nil {
			return 0, "", err
		}
		if _, err := io.WriteString(w, f.data); err != nil {
			return 0, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return 0, "", err
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, strings.TrimSpace(rec.Body.String()), nil
}

// formRequest builds the same multipart POST as postForm, with the
// caller's plan in planHeader, for handlers whose policy depends on it.
func formRequest(ctx context.Context, plan string, files ...formFile) *http.Request {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range files {
		w, _ := mw.CreateFormFile(upload.DefaultField, f.name)
		_, _ = io.WriteString(w, f.data)
	}
	_ = mw.Close()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(planHeader, plan)
	return req
}

// form posts the same three files to two handlers. The default stores all
// of a request's files or none of them; Partial stores what it can and
// lists the others with their codes. OnStored is where the application
// records the files; if it fails, the stored files are deleted.
// ds:def id=form-bav4dakx
func form(ctx context.Context, out io.Writer) error {
	files := []formFile{{"a.png", pngFile}, {"page.html", htmlFile}, {"b.pdf", pdfFile}}
	for _, partial := range []bool{false, true} {
		disk, err := newDisk()
		if err != nil {
			return err
		}
		recorded := 0
		h := upload.Handler(disk, policy, upload.HandlerOptions{
			MaxFiles: maxFilesPerRequest,
			Partial:  partial,
			OnStored: func(_ context.Context, _ *http.Request, stored []upload.Result) error {
				recorded = len(stored) // a real app inserts rows here
				return nil
			},
		})
		status, body, err := postForm(ctx, h, files...)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "form partial=%t: %d, stored %d, recorded %d\n", partial, status, count(ctx, disk), recorded)
		if !partial {
			fmt.Fprintf(out, "  body %s\n", body)
		}
	}
	return nil
}

// formFailures shows the JSON a Partial request answers when some files
// fail: each failure carries its own code.
// ds:def id=formfailures-8rzt54v2
func formFailures(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	h := upload.Handler(disk, policy, upload.HandlerOptions{MaxFiles: maxFilesPerRequest, Partial: true})
	_, body, err := postForm(ctx, h, formFile{"page.html", htmlFile}, formFile{"b.pdf", pdfFile})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  partial body: %s\n", body)
	return nil
}
