# The fs command-line tool, driven end to end

This program builds the `fs` command-line tool from [`cmd/fs`](../../cmd/fs) and runs it through a whole session: storing and listing files on a local disk, a recursive copy and a move that fails part-way, copies and signed URLs on an S3 bucket, signed URLs for a local disk, and the errors a script branches on. It prints each command and what `fs` answered, and the transcripts in the guide [The fs command-line tool](../../docs/integrations/cli.md) are its output.

## Run it

```sh
cd examples
go run ./integrations-cli
go test ./integrations-cli
```

It needs only the Go toolchain on `PATH`: it runs `go build` inside `cmd/fs` (a module of its own, so the core never depends on the cloud SDKs) into a temporary directory, serves an in-process fake S3 ([gofakes3](https://github.com/johannesboyne/gofakes3)) on a random local port, and runs every command from a fresh temporary work directory that it deletes at the end. No cloud account, Docker or network. The first run compiles `fs` (a few seconds with a warm module cache).

## What it does

The first lines of the output (the rest is in the guide, section by section):

```text
# local disk
$ fs put 'file:srv#reports/q3.txt' report.txt
key:          reports/q3.txt
size:         17
modified:     2026-10-08T13:16:59Z
content type: text/plain; charset=utf-8
etag:         18dc904ebd7e70c1-18dc904ebe0d6db7-11-814dbc6
visibility:   private
sha256:       522a52306ab093ef4b59cbb0c41a123af96bcb7837d6e68b7d26e875d8a2bf72
$ fs put 'file:srv#notes/hello.txt'
key:          notes/hello.txt
size:         17
modified:     2026-10-08T13:16:59Z
content type: text/plain; charset=utf-8
etag:         18dc904ebfecac8f-18dc904ec0459ff8-11-814dbca
visibility:   private
sha256:       a25c27baed0c36618ea226cee708e38908e6baa262f37ff032795869ff60a4f7
$ fs ls file:srv
  -  -    notes/
  -  -  reports/
$ fs ls -r file:srv
  17  2026-10-08 13:16:59  notes/hello.txt
  17  2026-10-08 13:16:59   reports/q3.txt
$ fs cat 'file:srv#notes/hello.txt'
hello from stdin
```

1. `put` stores a file and a stream from standard input, printing the SHA-256 computed while the bytes were written.
2. `ls` folds sub-directories into one entry each; `ls -r` lists every key.
3. `cp` to `mem://` streams across backends and keeps the digest; `mv` inside one DSN is a rename, so no digest is printed.
4. `cp -r -dry-run`, `cp -r` and `mv -r` copy and move a directory; a `mv -r` onto a key that is a file fails for every object, leaves the sources in place and exits 1, and `-json` turns that report into one JSON document.
5. On the fake S3, `cp`, `ls -r`, a signed PUT URL valid one minute and `check` (write, read back, compare, delete) all succeed.
6. `url` on a local disk is `unsupported` until `FS_SIGNING_SECRET` is set; then it signs with the application's `signedurl` scheme.
7. A missing key, a `..` key, a read-only DSN and an unknown DSN parameter exit 1 with their codes; a missing argument is a usage error and exits 2.

## The code

The build: `go build` runs inside `cmd/fs`, found by walking up from the working directory, so the program works from `examples` and from its own folder.

<!-- ds:block id=buildfs-auvuhrsk -->
**buildFS** · [`examples/integrations-cli/main.go:100-113`](main.go#L100-L113)

```go
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
```
<!-- /ds:block hash=f2f323 -->

The session: one `step` per command, in order. Locations are DSNs with the key after `#`, and each step adds only the environment it names.

<!-- ds:block id=session-9yducmrs -->
**session** · [`examples/integrations-cli/main.go:167-201`](main.go#L167-L201)

```go
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
```
<!-- /ds:block hash=220505 -->

Each step runs from the work directory; its standard output and error are printed together, then `(exit N)` when the code is not 0.

<!-- ds:block id=runstep-qxfm6t7j -->
**runStep** · [`examples/integrations-cli/main.go:207-230`](main.go#L207-L230)

```go
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
```
<!-- /ds:block hash=0aea72 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: builds `fs`, starts the fake S3, runs the session and prints the transcript |
| `main_test.go` | Runs the whole session and checks every line that does not change between runs (digests, listings, reports, error codes, exit codes) |

## Next

- The guide: [The fs command-line tool](../../docs/integrations/cli.md), and the tool's reference [cmd/fs/README.md](../../cmd/fs/README.md).
- [DSN parameters](../../docs/reference/dsn.md): every parameter a location accepts.
- [Configuration](../concepts-configuration): the same DSNs read from an application's environment.
