// Command integrations-cli builds the fs command-line tool (cmd/fs) and
// drives it through a session on a local disk, a recursive copy, an
// in-process fake S3 and a set of failures, printing each command and
// what it answered. The transcripts in docs/integrations/cli.md come from
// this program.
//
//	go run ./integrations-cli
//
// It needs the go toolchain on PATH (it runs go build on cmd/fs, its own
// module) and nothing else: no cloud account, no Docker, no network.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// Decisions of this demo, not of the tool.
const (
	// s3Bucket is the bucket created on the fake S3.
	s3Bucket = "uploads"
	// signingSecret stands in for the 32+ byte secret an application gives
	// signedurl.Key.Secret; fs reads it only from FS_SIGNING_SECRET.
	signingSecret = "demo-secret-of-at-least-thirty-two-bytes"
	// signingBase is where the application would mount fshttp.Signed.
	signingBase = "https://app.example.com/files"
	// readHeaderTimeout bounds the fake S3's request headers.
	readHeaderTimeout = 5 * time.Second
)

// files are the inputs the session stores, written into the work
// directory before the first command.
var files = map[string]string{
	"report.txt":  "Q3 revenue: 1.2M\n",
	"hello.txt":   "hello from stdin\n",
	"q4.txt":      "Q4 forecast\n",
	"summary.txt": "two quarters\n",
}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run builds fs, starts a fake S3 and runs every step of the session in
// a fresh temporary directory, printing a transcript to out.
func run(ctx context.Context, out io.Writer) error {
	tmp, err := os.MkdirTemp("", "fs-cli-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	bin, err := buildFS(ctx, filepath.Join(tmp, "bin"))
	if err != nil {
		return err
	}
	work := filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o750); err != nil {
		return err
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o600); err != nil {
			return err
		}
	}
	endpoint, stop, err := startFakeS3()
	if err != nil {
		return err
	}
	defer stop()

	for _, s := range session(endpoint) {
		if err := runStep(ctx, out, bin, work, s); err != nil {
			return err
		}
	}
	return nil
}

// buildFS compiles cmd/fs into dir and returns the binary's path. fs is
// its own module (so the core never depends on the cloud SDKs), and
// go build runs inside it, with its own go.mod.
// ds:def id=buildfs-auvuhrsk
func buildFS(ctx context.Context, dir string) (string, error) {
	src, err := cmdFSDir()
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "fs")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build cmd/fs: %w\n%s", err, out)
	}
	return bin, nil
}

// cmdFSDir finds cmd/fs by walking up from the working directory, so the
// program works from examples (go run) and from its own folder (go test).
func cmdFSDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, "cmd", "fs")
		if _, err := os.Stat(filepath.Join(candidate, "go.mod")); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("cmd/fs not found above the working directory: run from the repository")
		}
		dir = parent
	}
}

// startFakeS3 serves an in-memory S3 with the demo bucket on a free local
// port and returns its endpoint.
func startFakeS3() (string, func(), error) {
	backend := s3mem.New()
	if err := backend.CreateBucket(s3Bucket); err != nil {
		return "", nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: gofakes3.New(backend).Server(), ReadHeaderTimeout: readHeaderTimeout}
	go func() { _ = srv.Serve(ln) }()
	return "http://" + ln.Addr().String(), func() { _ = srv.Close() }, nil
}

// step is one fs invocation of the session.
type step struct {
	// title, when set, prints a heading before the command.
	title string
	// args are fs's arguments.
	args []string
	// stdin, when set, is piped to fs.
	stdin string
	// env adds variables to fs's environment.
	env []string
}

// session is every command the guide shows, in order. A location is a
// DSN with the key after #; file:srv is the directory srv under the work
// directory.
// ds:def id=session-9yducmrs
func session(endpoint string) []step {
	bucket := "s3://" + s3Bucket + "?region=us-east-1&path_style=true&endpoint=" + endpoint
	aws := []string{"AWS_ACCESS_KEY_ID=example", "AWS_SECRET_ACCESS_KEY=example"}
	signing := []string{"FS_SIGNING_SECRET=" + signingSecret, "FS_SIGNING_BASE=" + signingBase}
	return []step{
		{title: "local disk", args: []string{"put", "file:srv#reports/q3.txt", "report.txt"}},
		{args: []string{"put", "file:srv#notes/hello.txt"}, stdin: files["hello.txt"]},
		{args: []string{"ls", "file:srv"}},
		{args: []string{"ls", "-r", "file:srv"}},
		{args: []string{"cat", "file:srv#notes/hello.txt"}},
		{args: []string{"cp", "file:srv#reports/q3.txt", "mem://scratch#q3.txt"}},
		{args: []string{"mv", "file:srv#notes/hello.txt", "file:srv#notes/hi.txt"}},
		{args: []string{"-json", "ls", "file:srv#notes"}},
		{title: "directories", args: []string{"put", "file:srv#reports/2026/q4.txt", "q4.txt"}},
		{args: []string{"put", "file:srv#reports/summary.txt", "summary.txt"}},
		{args: []string{"cp", "-r", "-dry-run", "file:srv#reports", "file:bak#reports"}},
		{args: []string{"cp", "-r", "file:srv#reports", "file:bak#reports"}},
		{args: []string{"mv", "-r", "file:srv#reports/2026", "file:srv#archive/2026"}},
		{args: []string{"put", "file:bak#moved", "summary.txt"}},
		{args: []string{"mv", "-r", "file:srv#archive", "file:bak#moved"}},
		{args: []string{"-json", "mv", "-r", "file:srv#archive", "file:bak#moved"}},
		{title: "s3", args: []string{"cp", "file:srv#reports/q3.txt", bucket + "#reports/q3.txt"}, env: aws},
		{args: []string{"ls", "-r", bucket + "#reports"}, env: aws},
		{args: []string{"-json", "url", "-put", "-expires", "1m", bucket + "#uploads/new.pdf"}, env: aws},
		{args: []string{"check", bucket}, env: aws},
		{title: "signed URLs for local disks", args: []string{"url", "file:srv#notes/hi.txt"}},
		{args: []string{"url", "-disk", "media", "file:srv#notes/hi.txt"}, env: signing},
		{title: "errors", args: []string{"-json", "stat", "file:srv#reports/missing.txt"}},
		{args: []string{"cat", "file:srv#../etc/passwd"}},
		{args: []string{"put", "file:srv?readonly=true#x.txt", "report.txt"}},
		{args: []string{"ls", "file:srv?bucket=x"}},
		{args: []string{"cp", "file:srv#notes/hi.txt"}},
		{args: []string{"rm", "-r", "file:srv#notes"}},
	}
}

// runStep runs one step from the work directory and prints the command,
// its standard output and error, and its exit code when it is not 0.
// Only the variables the step names are added to the environment.
// ds:def id=runstep-qxfm6t7j
func runStep(ctx context.Context, out io.Writer, bin, work string, s step) error {
	if s.title != "" {
		fmt.Fprintf(out, "# %s\n", s.title)
	}
	fmt.Fprintf(out, "$ fs %s\n", quoteArgs(s.args))
	cmd := exec.CommandContext(ctx, bin, s.args...)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), s.env...)
	if s.stdin != "" {
		cmd.Stdin = strings.NewReader(s.stdin)
	}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	_, _ = out.Write(buf.Bytes())
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		fmt.Fprintf(out, "(exit %d)\n", exit.ExitCode())
	case err != nil:
		return err
	}
	return nil
}

// quoteArgs prints args as a shell would need them: single-quoted when
// they hold a character the shell treats specially (#, ?, &).
func quoteArgs(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, "#?&") {
			a = "'" + a + "'"
		}
		q[i] = a
	}
	return strings.Join(q, " ")
}
