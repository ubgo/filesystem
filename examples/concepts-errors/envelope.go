package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
)

// problemType is the media type of an RFC 9457 problem document.
const problemType = "application/problem+json"

// problem is an fshttp.ErrorWriter that answers RFC 9457 problem+json,
// turns a plan limit (quota_exceeded) into 402 and logs the cause of a
// 5xx. Every handler of the library takes it in its options.
// ds:def id=problem-k2n8v4wq
func problem(w http.ResponseWriter, r *http.Request, status int, err error) {
	if filesystem.CodeOf(err) == filesystem.CodeQuotaExceeded {
		status = http.StatusPaymentRequired // a plan limit, not a full disk
	}
	if status >= http.StatusInternalServerError {
		slog.ErrorContext(r.Context(), "storage failed", "err", err) // the full error, for logs only
	}
	w.Header().Set("Content-Type", problemType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": status,
		"title":  fshttp.ClientMessage(err), // never err.Error(): it names the disk and the key
		"code":   filesystem.CodeOf(err),
	})
}

// overPlan stands in for a plan check that refuses the upload.
func overPlan(context.Context, *filesystem.Disk, upload.Result) error {
	return filesystem.Errorf(filesystem.CodeQuotaExceeded, "the free plan holds 1 GB")
}

// envelope sends the same refused upload to a form handler with the
// default shape and with problem, then asks a signed-URL handler with
// problem for a link that was never signed.
// ds:def id=envelope-r5t9x3mc
func envelope(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		return err
	}
	policy := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"text/plain"}, Key: upload.RandomKey("docs"), Inspect: overPlan}
	for _, c := range []struct {
		label string
		opts  upload.HandlerOptions
	}{{"default", upload.HandlerOptions{}}, {"problem", upload.HandlerOptions{Errors: problem}}} {
		rec, err := postFile(ctx, upload.Handler(disk, policy, c.opts))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "upload over the plan, %s: %d %s %s", c.label, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	signer, err := signedurl.New("https://app.example.com/files", []signedurl.Key{{ID: "k1", Secret: []byte("0123456789abcdef0123456789abcdef")}})
	if err != nil {
		return err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return err
	}
	files := fshttp.Signed(disks, signer, fshttp.SignedOptions{Errors: problem})
	rec := httptest.NewRecorder()
	files.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/media/docs/report.txt", nil))
	fmt.Fprintf(out, "unsigned download, problem: %d %s %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	return nil
}

// postFile posts one small text file as a multipart form to h.
func postFile(ctx context.Context, h http.Handler) (*httptest.ResponseRecorder, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile(upload.DefaultField, "notes.txt")
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(fw, strings.NewReader("meeting notes\n")); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, nil
}
