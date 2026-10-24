// Command recipe-virus-scanning scans every upload for malware before it is
// accepted: ClamAV's clamd when CLAMD_ADDR is set, otherwise a stand-in
// that only recognises the EICAR test file, so the demo runs without
// Docker. Images are also checked to have a readable header. A file that
// fails either check is deleted and the request fails; a scanner that is
// down refuses uploads instead of letting them through unscanned.
//
//	go run ./recipe-virus-scanning                            (stand-in scanner)
//	CLAMD_ADDR=127.0.0.1:3310 go run ./recipe-virus-scanning  (real clamd)
//
// Routes:
//
//	POST /uploads   multipart form, field "file", up to 5 files
//	GET  /readyz    200 when the scanner answers
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/examples/internal/serve"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/scan"
	"github.com/ubgo/filesystem/upload"
)

// Decisions of this app, not of the library.
const (
	// listenAddr is where the demo server listens.
	listenAddr = "localhost:8111"
	// envClamdAddr names the variable holding clamd's TCP address.
	envClamdAddr = "CLAMD_ADDR"
	// maxUploadBytes caps one file. clamd's StreamMaxLength must be at
	// least this large, or such files fail with CodeTooLarge.
	maxUploadBytes = 25 << 20
	// maxFilesPerRequest caps one request.
	maxFilesPerRequest = 5
	// scanTimeout bounds one scan, connection included.
	scanTimeout = 30 * time.Second
	// readyTimeout bounds the readiness probe.
	readyTimeout = 2 * time.Second
	// readHeaderTimeout bounds a slow client's headers.
	readHeaderTimeout = 10 * time.Second
)

// eicarSignature is the industry-standard antivirus test string: harmless,
// and reported by every scanner as infected. The stand-in scanner looks for
// exactly this and nothing else.
const eicarSignature = `X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`

// eicarName is the signature name the stand-in reports, as ClamAV does.
const eicarName = "Eicar-Test-Signature"

// pinger is a scanner that can report whether it is reachable.
// scan.Clamd is one; the stand-in is not.
type pinger interface {
	Ping(ctx context.Context) error
}

// newScanner returns clamd when an address is configured, else the
// stand-in. The stand-in is loud about itself, so nobody mistakes "nothing
// found" for a real scan.
// ds:def id=newscanner-u7ybdejf
func newScanner(lookup func(string) (string, bool)) scan.Scanner {
	if addr, ok := lookup(envClamdAddr); ok && addr != "" {
		return scan.Clamd{Network: "tcp", Address: addr, Timeout: scanTimeout}
	}
	fmt.Fprintf(os.Stderr, "WARN: %s is not set: using a stand-in scanner that detects only the EICAR test file\n", envClamdAddr)
	return scan.ScannerFunc(eicarOnly)
}

// eicarOnly is the stand-in scanner: infected when the stream contains the
// EICAR test string. It is a test double, not protection.
// ds:def id=eicaronly-9nrbej47
func eicarOnly(_ context.Context, r io.Reader) (scan.Verdict, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxUploadBytes+1))
	if err != nil {
		return scan.Verdict{}, err
	}
	if bytes.Contains(data, []byte(eicarSignature)) {
		return scan.Verdict{Clean: false, Signature: eicarName}, nil
	}
	return scan.Verdict{Clean: true}, nil
}

// imageHeader is a second inspection: an upload whose type says image must
// have a header imaging can read. It catches truncated or corrupt images
// before anything tries to thumbnail them.
// ds:def id=imageheader-rn6ydg6y
func imageHeader(ctx context.Context, disk *filesystem.Disk, r upload.Result) error {
	if !strings.HasPrefix(r.ContentType, "image/") {
		return nil
	}
	rc, err := disk.Read(ctx, r.Key)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	_, err = imaging.Probe(rc)
	return err
}

// newPolicy is the upload policy with both inspections, the scan first.
// Inspect runs on every stored file before success is reported; an error
// deletes the file and fails the upload.
// ds:def id=newpolicy-52qncapy
func newPolicy(s scan.Scanner) upload.Policy {
	return upload.Policy{
		MaxSize: maxUploadBytes,
		Allowed: []string{"image/png", "image/jpeg", "application/pdf", "text/plain", "application/zip"},
		Key:     upload.RandomKey("uploads"),
		Inspect: upload.Inspectors(scan.Inspect(s), imageHeader),
	}
}

// ready answers 200 when the scanner is reachable and 503 when it is not,
// for a load balancer's readiness probe: an instance whose scanner is down
// refuses every upload, so it should not receive them.
// ds:def id=ready-dxq3dmmu
func ready(s scan.Scanner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.(pinger)
		if !ok {
			_, _ = io.WriteString(w, "ok (stand-in scanner)\n")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()
		if err := p.Ping(ctx); err != nil {
			http.Error(w, "scanner unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	}
}

// New wires the upload endpoint over a data directory and a scanner.
// ds:def id=new-d27eer4u
func New(dataDir string, s scan.Scanner) (http.Handler, *filesystem.Disk, error) {
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk("uploads", drv)
	if err != nil {
		return nil, nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("POST /uploads", upload.Handler(disk, newPolicy(s), upload.HandlerOptions{MaxFiles: maxFilesPerRequest}))
	mux.HandleFunc("GET /readyz", ready(s))
	return mux, disk, nil
}

func main() {
	// Ctrl+C or SIGTERM stops the server cleanly, so the deferred
	// clean-up below runs.
	ctx, stop := serve.Context()
	defer stop()
	dir, err := os.MkdirTemp("", "recipe-virus-scanning-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	h, _, err := New(dir, newScanner(os.LookupEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("listening on http://"+listenAddr, "data in", dir)
	srv := &http.Server{Addr: listenAddr, Handler: h, ReadHeaderTimeout: readHeaderTimeout}
	if err := serve.UntilDone(ctx, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
