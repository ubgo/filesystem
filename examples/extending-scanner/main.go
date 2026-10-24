// Command extending-scanner writes a malware scanner of its own: a client
// for an HTTP scanning service (the shape of most cloud scanning APIs),
// plugged into an upload policy with scan.Inspect so every upload path
// scans before it reports success. It also wraps a scanner to fail open,
// for the application that prefers to stay up when the scanner is down.
//
//	go run ./extending-scanner
//
// The scanning service is simulated in-process; it flags the EICAR test
// string, which every antivirus product treats as a virus by convention.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/scan"
	"github.com/ubgo/filesystem/upload"
)

// eicar is the standard antivirus test string: harmless, and detected as
// a virus by every scanner.
const eicar = `X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`

// Settings of the example.
const (
	diskName      = "media"
	maxUploadSize = 10 << 20
	// maxVerdictBytes caps the service's JSON reply, so a misbehaving
	// service cannot make the scanner read without end.
	maxVerdictBytes = 64 << 10
)

// HTTPScanner sends the file to a scanning service and reads its verdict.
// It implements scan.Scanner: an error means the scan did not complete
// and says nothing about the file, which scan.Inspect turns into
// CodeUnavailable, so the upload is refused rather than let through.
// ds:def id=httpscanner-3zvefzbc
type HTTPScanner struct {
	URL    string
	Client *http.Client
}

// verdictReply is the service's JSON answer.
type verdictReply struct {
	Clean     bool   `json:"clean"`
	Signature string `json:"signature"`
}

// Scan implements scan.Scanner. It streams the file in the request body,
// so a large upload is never held in memory.
// ds:def id=httpscanner-scan-mxa78qxc
func (s HTTPScanner) Scan(ctx context.Context, r io.Reader) (scan.Verdict, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, r)
	if err != nil {
		return scan.Verdict{}, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := s.Client.Do(req)
	if err != nil {
		return scan.Verdict{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return scan.Verdict{}, fmt.Errorf("scanner answered %s", resp.Status)
	}
	var v verdictReply
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxVerdictBytes)).Decode(&v); err != nil {
		return scan.Verdict{}, err
	}
	return scan.Verdict{Clean: v.Clean, Signature: v.Signature}, nil
}

// FailOpen wraps a scanner so that a scan that cannot run lets the file
// through, after telling onError. The library fails closed on purpose;
// this is the application's decision to make, and it should alert on
// every call of onError, because each one is an unscanned file.
// ds:def id=failopen-6aq3sfyu
func FailOpen(s scan.Scanner, onError func(error)) scan.Scanner {
	return scan.ScannerFunc(func(ctx context.Context, r io.Reader) (scan.Verdict, error) {
		v, err := s.Scan(ctx, r)
		if err != nil {
			onError(err)
			return scan.Verdict{Clean: true}, nil
		}
		return v, nil
	})
}

// policyWith returns the upload policy with the scanner as its Inspect
// step: it runs on every stored upload (form, import, tus, presign
// Confirm, Uppy complete) before success is reported, and its error
// deletes the file.
// ds:def id=policywith-gx22ue2z
func policyWith(s scan.Scanner) upload.Policy {
	return upload.Policy{
		MaxSize: maxUploadSize,
		Allowed: []string{"text/plain", "application/pdf", "image/*"},
		Inspect: scan.Inspect(s),
	}
}

// scanPath is where the simulated service accepts files; any other path
// answers 404, which stands in for a service that is down.
const scanPath = "/scan"

// fakeService answers like a scanning API: 200 with a verdict.
func fakeService() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+scanPath, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "unreadable body", http.StatusBadRequest)
			return
		}
		v := verdictReply{Clean: true}
		if bytes.Contains(body, []byte("EICAR-STANDARD-ANTIVIRUS-TEST-FILE")) {
			v = verdictReply{Signature: "Eicar-Test-Signature"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	})
	return mux
}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run uploads a clean file and the EICAR string, shows the scanner
// failing closed and, wrapped, failing open, then adds a moderation hash
// list beside it.
func run(ctx context.Context, out io.Writer) error {
	svc := httptest.NewServer(fakeService())
	defer svc.Close()
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return err
	}
	scanner := HTTPScanner{URL: svc.URL + scanPath, Client: svc.Client()}
	for _, body := range []string{"quarterly numbers\n", eicar} {
		res, err := upload.Receive(ctx, disk, strings.NewReader(body), "file.txt", policyWith(scanner))
		fmt.Fprintf(out, "upload: key=%q err=%v\n", res.Key, err)
	}
	down := HTTPScanner{URL: svc.URL + "/down", Client: svc.Client()}
	_, err = upload.Receive(ctx, disk, strings.NewReader("hello\n"), "file.txt", policyWith(down))
	fmt.Fprintf(out, "scanner down, fail closed: %v\n", err)
	open := FailOpen(down, func(err error) { fmt.Fprintf(out, "alert: unscanned upload: %v\n", err) })
	res, err := upload.Receive(ctx, disk, strings.NewReader("hello\n"), "file.txt", policyWith(open))
	fmt.Fprintf(out, "scanner down, fail open: stored=%t err=%v\n", res.Key != "", err)
	return moderation(ctx, out, disk, scanner)
}
