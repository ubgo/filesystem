package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/signedurl"
)

type result struct {
	code           int
	stdout, stderr string
}

func fsRun(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	return fsRunEnv(t, nil, stdin, args...)
}

// fsRunEnv runs fs with env as its whole environment.
func fsRunEnv(t *testing.T, env map[string]string, stdin string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(t.Context(), args, func(k string) string { return env[k] }, strings.NewReader(stdin), &out, &errb)
	return result{code, out.String(), errb.String()}
}

func loc(dir, key string) string { return "file://" + dir + "#" + key }

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if r := fsRun(t, "hello from stdin", "put", loc(dir, "docs/a.txt")); r.code != exitOK || !strings.Contains(r.stdout, "key:          docs/a.txt") {
		t.Fatalf("put = %+v", r)
	}
	src := filepath.Join(t.TempDir(), "local.png")
	_ = os.WriteFile(src, []byte("\x89PNG\r\n\x1a\nimage"), 0o600)
	if r := fsRun(t, "", "-json", "put", "-type", "image/x-forced", loc(dir, "img/b.png"), src); r.code != exitOK || !strings.Contains(r.stdout, `"key": "img/b.png"`) {
		t.Fatalf("put file = %+v", r)
	}
	if r := fsRun(t, "", "cat", loc(dir, "docs/a.txt")); r.stdout != "hello from stdin" {
		t.Errorf("cat = %+v", r)
	}
	if r := fsRun(t, "", "stat", loc(dir, "docs/a.txt")); !strings.Contains(r.stdout, "size:         16") {
		t.Errorf("stat = %+v", r)
	}
	r := fsRun(t, "", "ls", loc(dir, ""))
	if r.code != exitOK || !strings.Contains(r.stdout, "docs/") || !strings.Contains(r.stdout, "img/") {
		t.Errorf("ls = %+v", r)
	}
	r = fsRun(t, "", "-json", "ls", "-r", loc(dir, ""))
	var entries []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &entries); err != nil || len(entries) != 2 {
		t.Errorf("ls -json = %s (%v)", r.stdout, err)
	}
	if r := fsRun(t, "", "-json", "ls", loc(dir, "nothing")); strings.TrimSpace(r.stdout) != "[]" {
		t.Errorf("empty ls -json = %q", r.stdout)
	}

	other := t.TempDir()
	if r := fsRun(t, "", "cp", loc(dir, "docs/a.txt"), loc(other, "copied.txt")); r.code != exitOK {
		t.Fatalf("cp = %+v", r)
	}
	if r := fsRun(t, "", "mv", loc(dir, "docs/a.txt"), loc(other, "moved.txt")); r.code != exitOK {
		t.Fatalf("mv = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "docs", "a.txt")); err == nil {
		t.Error("mv left the source")
	}
	if r := fsRun(t, "", "-json", "rm", loc(other, "copied.txt")); strings.TrimSpace(r.stdout) != "{\n  \"ok\": true\n}" {
		t.Errorf("rm -json = %q", r.stdout)
	}
	if r := fsRun(t, "", "rm", "-r", loc(dir, "img")); r.code != exitOK {
		t.Errorf("rm -r = %+v", r)
	}
	if r := fsRun(t, "", "check", "file://"+dir); r.code != exitOK || strings.TrimSpace(r.stdout) != "ok" {
		t.Errorf("check = %+v", r)
	}
	if r := fsRun(t, "", "-json", "check", "file://"+dir); !strings.Contains(r.stdout, `"ok": true`) {
		t.Errorf("check -json = %+v", r)
	}
}

func TestFailures(t *testing.T) {
	dir := t.TempDir()
	r := fsRun(t, "", "stat", loc(dir, "missing"))
	if r.code != exitFailed || !strings.Contains(r.stderr, "not_found") {
		t.Errorf("missing = %+v", r)
	}
	r = fsRun(t, "", "-json", "cat", loc(dir, "missing"))
	if r.code != exitFailed || !strings.Contains(r.stdout, `"code":"not_found"`) {
		t.Errorf("missing -json = %+v", r)
	}
	if r := fsRun(t, "", "url", loc(dir, "x")); r.code != exitFailed || !strings.Contains(r.stderr, "unsupported") {
		t.Errorf("url on local = %+v", r)
	}
	if r := fsRun(t, "", "ls", "nope://x"); r.code != exitFailed {
		t.Errorf("unknown scheme = %+v", r)
	}
	if r := fsRun(t, "", "put", loc(dir, "x"), "/does/not/exist"); r.code != exitFailed {
		t.Errorf("missing source file = %+v", r)
	}
	if r := fsRun(t, "", "cp", loc(dir, "missing"), loc(dir, "y")); r.code != exitFailed {
		t.Errorf("cp missing = %+v", r)
	}
	if r := fsRun(t, "", "cp", loc(dir, "missing"), loc(t.TempDir(), "y")); r.code != exitFailed || !strings.Contains(r.stderr, "not_found") {
		t.Errorf("cp missing across disks = %+v", r)
	}
	if r := fsRun(t, "", "cp", loc(dir, "a"), "nope://"); r.code != exitFailed {
		t.Errorf("cp to a bad target = %+v", r)
	}
	if r := fsRun(t, "", "rm", loc(dir, "../x")); r.code != exitFailed {
		t.Errorf("rm traversal = %+v", r)
	}
	ro := "file://" + dir + "?readonly=true"
	if r := fsRun(t, "", "check", ro); r.code != exitFailed {
		t.Errorf("check read-only = %+v", r)
	}
	for _, cmd := range [][]string{{"stat", "nope://"}, {"cat", "nope://"}, {"put", "nope://"}, {"rm", "nope://"}, {"url", "nope://"}, {"check", "nope://"}} {
		if r := fsRun(t, "", cmd...); r.code != exitFailed {
			t.Errorf("%v = %+v", cmd, r)
		}
	}
}

func TestUsage(t *testing.T) {
	if r := fsRun(t, ""); r.code != exitUsage || !strings.Contains(r.stdout, "Commands:") {
		t.Errorf("no args = %+v", r)
	}
	if r := fsRun(t, "", "help"); r.code != exitOK || !strings.Contains(r.stdout, "Examples:") {
		t.Errorf("help = %+v", r)
	}
	if r := fsRun(t, "", "-bogus"); r.code != exitUsage {
		t.Errorf("bad global flag = %+v", r)
	}
	if r := fsRun(t, "", "frobnicate"); r.code != exitUsage || !strings.Contains(r.stderr, "unknown command") {
		t.Errorf("unknown command = %+v", r)
	}
	for _, args := range [][]string{{"ls"}, {"ls", "-x", "a"}, {"cp", "a"}, {"put"}, {"url", "-expires", "soon", "a"}, {"stat", "a", "b"}, {"cat"}, {"rm"}, {"check"}, {"mv", "a"}} {
		if r := fsRun(t, "", args...); r.code != exitUsage || !strings.Contains(r.stderr, "usage: fs ") {
			t.Errorf("%v = %+v", args, r)
		}
	}
}

func TestSignedURLOnS3(t *testing.T) {
	srv := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
	defer srv.Close()
	client := s3.New(s3.Options{BaseEndpoint: aws.String(srv.URL), UsePathStyle: true, Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("a", "b", "")})
	if _, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("cli-test")}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "a")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "b")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")
	dsn := "s3://cli-test?region=us-east-1&path_style=true&endpoint=" + url.QueryEscape(srv.URL)
	if r := fsRun(t, "data", "put", dsn+"#k.txt"); r.code != exitOK {
		t.Fatalf("put to s3 = %+v", r)
	}
	r := fsRun(t, "", "url", dsn+"#k.txt")
	if r.code != exitOK || !strings.Contains(r.stdout, "X-Amz-Signature") {
		t.Errorf("url = %+v", r)
	}
	r = fsRun(t, "", "-json", "url", "-put", "-expires", "1m", dsn+"#k.txt")
	if r.code != exitOK || !strings.Contains(r.stdout, `"method": "PUT"`) || !strings.Contains(r.stdout, `"expires_at"`) {
		t.Errorf("url -json = %+v", r)
	}
}

func TestMoreFailuresAndListings(t *testing.T) {
	dir := t.TempDir()
	_ = fsRun(t, "x", "put", loc(dir, "a/b.txt"))
	if r := fsRun(t, "", "ls", "-r", loc(dir, "")); r.code != exitOK || !strings.Contains(r.stdout, "a/b.txt") || !strings.Contains(r.stdout, "1") {
		t.Errorf("ls -r = %+v", r)
	}
	if r := fsRun(t, "y", "put", "file://"+dir+"?readonly=true#c.txt"); r.code != exitFailed || !strings.Contains(r.stderr, "read_only") {
		t.Errorf("put to read-only = %+v", r)
	}
	if r := fsRun(t, "", "cp", "nope://x#a", loc(dir, "b")); r.code != exitFailed {
		t.Errorf("cp from a bad source = %+v", r)
	}
	if os.Geteuid() != 0 {
		locked := filepath.Join(dir, "locked")
		_ = os.Mkdir(locked, 0o700)
		_ = os.WriteFile(filepath.Join(locked, "f"), []byte("x"), 0o600)
		_ = os.Chmod(locked, 0o000)
		defer func() { _ = os.Chmod(locked, 0o700) }()
		if r := fsRun(t, "", "ls", "-r", loc(dir, "")); r.code != exitFailed {
			t.Errorf("ls over an unreadable directory = %+v", r)
		}
	}
}

// TestMoveWithinOneDSNRenames pins that mv between two keys of one location
// is Disk.Move (a rename on local disk: the file keeps its identity), not a
// stream and a delete, and that cp there is Disk.Copy.
func TestMoveWithinOneDSNRenames(t *testing.T) {
	dir := t.TempDir()
	if r := fsRun(t, "payload", "put", loc(dir, "a.txt")); r.code != exitOK {
		t.Fatalf("put = %+v", r)
	}
	before, err := os.Stat(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if r := fsRun(t, "", "mv", loc(dir, "a.txt"), loc(dir, "sub/b.txt")); r.code != exitOK || !strings.Contains(r.stdout, "key:          sub/b.txt") {
		t.Fatalf("mv = %+v", r)
	}
	after, err := os.Stat(filepath.Join(dir, "sub", "b.txt"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("mv within one disk did not rename the file (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !os.IsNotExist(err) {
		t.Errorf("mv left the source: %v", err)
	}
	if r := fsRun(t, "", "cp", loc(dir, "sub/b.txt"), loc(dir, "c.txt")); r.code != exitOK {
		t.Fatalf("cp within one disk = %+v", r)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "c.txt")); string(got) != "payload" {
		t.Errorf("cp within one disk stored %q", got)
	}
	if r := fsRun(t, "", "mv", loc(dir, "missing"), loc(dir, "d.txt")); r.code != exitFailed || !strings.Contains(r.stderr, "not_found") {
		t.Errorf("mv of a missing key = %+v", r)
	}
	if r := fsRun(t, "", "mv", "nope://x#a", "nope://x#b"); r.code != exitFailed {
		t.Errorf("mv within an unknown scheme = %+v", r)
	}
}

func TestSignedURLOnLocalWithASecret(t *testing.T) {
	dir := t.TempDir()
	secret := strings.Repeat("s", 32)
	env := map[string]string{envSigningSecret: secret, envSigningBase: "https://app.test/files"}
	verifier, err := signedurl.New("https://app.test/files", []signedurl.Key{{ID: defaultSigningKeyID, Secret: []byte(secret)}})
	if err != nil {
		t.Fatal(err)
	}
	verify := func(raw, disk, key string, method filesystem.HTTPMethod) {
		t.Helper()
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || !strings.HasPrefix(u.Path, "/files/"+disk+"/") {
			t.Fatalf("signed URL %q (%v) is not under /files/%s/", raw, err, disk)
		}
		if _, err := verifier.Verify(disk, key, method, u.Query()); err != nil {
			t.Fatalf("the application cannot verify %q: %v", raw, err)
		}
	}
	r := fsRunEnv(t, env, "", "url", loc(dir, "docs/a.pdf"))
	if r.code != exitOK {
		t.Fatalf("url = %+v", r)
	}
	verify(r.stdout, diskName, "docs/a.pdf", filesystem.MethodGet)

	// -disk names the disk the application serves; -base overrides the env.
	r = fsRunEnv(t, env, "", "-json", "url", "-put", "-disk", "media", "-base", "https://app.test/files", "mem://x#up.bin")
	var out urlOutput
	if r.code != exitOK || json.Unmarshal([]byte(r.stdout), &out) != nil || out.Method != "PUT" {
		t.Fatalf("url -put -disk = %+v", r)
	}
	verify(out.URL, "media", "up.bin", filesystem.MethodPut)

	// A key ID from the environment is the one the URL names.
	r = fsRunEnv(t, map[string]string{envSigningSecret: secret, envSigningBase: "https://app.test/files", envSigningKeyID: "2026-10"}, "", "url", loc(dir, "a"))
	if r.code != exitOK || !strings.Contains(r.stdout, "kid=2026-10") {
		t.Errorf("url with a key ID = %+v", r)
	}

	// Misconfiguration fails clearly.
	if r := fsRunEnv(t, map[string]string{envSigningSecret: "tiny-secret", envSigningBase: "https://app.test/files"}, "", "url", loc(dir, "a")); r.code != exitFailed || !strings.Contains(r.stderr, "invalid_argument") || strings.Contains(r.stderr, "tiny-secret") {
		t.Errorf("short secret = %+v", r)
	}
	if r := fsRunEnv(t, env, "", "url", "-expires", "999h", loc(dir, "a")); r.code != exitFailed || !strings.Contains(r.stderr, "invalid_argument") || strings.Contains(r.stderr, envSigningSecret) {
		t.Errorf("expiry over the limit = %+v", r)
	}
	if r := fsRunEnv(t, map[string]string{envSigningSecret: secret}, "", "url", loc(dir, "a")); r.code != exitFailed || !strings.Contains(r.stderr, "unsupported") || !strings.Contains(r.stderr, envSigningBase) {
		t.Errorf("secret without a base = %+v", r)
	}
	if r := fsRun(t, "", "-json", "url", loc(dir, "a")); r.code != exitFailed || !strings.Contains(r.stdout, `"code":"unsupported"`) || !strings.Contains(r.stdout, envSigningSecret) {
		t.Errorf("no secret = %+v", r)
	}
}

func TestEveryAdapterSchemeIsRegistered(t *testing.T) {
	for _, dsn := range []string{"gs://", "azblob://", "sftp://"} {
		r := fsRun(t, "", "ls", dsn)
		if r.code != exitFailed || strings.Contains(r.stderr, "no opener registered") || !strings.Contains(r.stderr, "invalid_argument") {
			t.Errorf("ls %s = %+v, want the adapter's own refusal", dsn, r)
		}
	}
}

func TestPutFromAPipe(t *testing.T) {
	dir := t.TempDir()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = w.WriteString("piped")
		_ = w.Close()
	}()
	defer func() { _ = r.Close() }()
	var out, errb bytes.Buffer
	if code := run(t.Context(), []string{"put", loc(dir, "p.txt")}, func(string) string { return "" }, r, &out, &errb); code != exitOK {
		t.Fatalf("put from a pipe = %d %s", code, errb.String())
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "p.txt")); string(got) != "piped" {
		t.Errorf("stored %q", got)
	}
}
