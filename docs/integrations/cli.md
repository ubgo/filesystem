# The fs command-line tool

`fs` works with any disk the library opens, from the shell: list a directory, read or write an object, copy between backends, sign a URL, and check that a bucket and its credentials work. It is the same `Mux`, DSNs and `Disk` your application uses, so a DSN that works in `fs` works in your config, and an error `fs` prints is the error your app would get.

Use it to inspect a bucket during an incident, to copy a few files between local disk and S3, to hand someone a signed link, or as a deploy step that fails when storage is misconfigured.

## Install

`fs` is its own module, `github.com/ubgo/filesystem/cmd/fs`, so the core never depends on the cloud SDKs it uses. The library is pre-release and that module's `go.mod` points at the working tree with `replace` directives, which `go install …@latest` refuses, so build it from a clone:

```sh
cd cmd/fs
go build -o fs .
```

Check: `./fs` prints the usage and exits 2.

## Locations

Every command takes a location: a DSN, with the key in its fragment after `#`.

| Location | Disk | Key |
|---|---|---|
| `file:srv#reports/q3.txt` | the directory `srv`, relative to the working directory | `reports/q3.txt` |
| `file:///var/app/uploads#avatars` | `/var/app/uploads` | `avatars` (a directory for `ls`) |
| `s3://media?region=us-east-1#reports/q3.pdf` | the S3 bucket `media` | `reports/q3.pdf` |
| `s3://media?region=auto&endpoint=https://<account>.r2.cloudflarestorage.com#a.png` | an R2 bucket | `a.png` |
| `mem://scratch#a.txt` | a memory disk that lives for one command | `a.txt` |
| `gs://media#a.png` | the GCS bucket `media` | `a.png` |
| `azblob://media#a.png` | the Azure container `media` | `a.png` |
| `sftp://deploy@files.example.com/srv/files?identity=/home/me/.ssh/id_ed25519&known_hosts=/home/me/.ssh/known_hosts#a.png` | `/srv/files` on an SFTP server | `a.png` |

`fs` registers thirteen schemes: `file`, `mem`, `s3`, `gs`, `azblob`, `sftp`, `ftp`, `ftps`, `ftps-implicit`, `webdav`, `webdavs`, `cloudinary` and `vercelblob`. Every parameter of those adapters and of the Mux works in a location ([DSN parameters](../reference/dsn.md)), including `prefix=`, `readonly=true` and an S3 provider's `endpoint`, `path_style` and `conditional_write`. S3 credentials come from the standard AWS chain: `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, `AWS_PROFILE`, or an instance role. GCS uses Application Default Credentials, Azure the connection string in `AZURE_STORAGE_CONNECTION_STRING`, SFTP a key file or the variable `password_env` names, FTP the variable its own `password_env` names ([FTP](../adapters/ftp.md), [WebDAV](../adapters/webdav.md)), Cloudinary the API secret in the variable `api_secret_env` names ([Cloudinary](../adapters/cloudinary.md)), and Vercel Blob the token in `BLOB_READ_WRITE_TOKEN` or the variable `token_env` names ([Vercel Blob](../adapters/vercelblob.md)). Quote locations in the shell: `?`, `&` and `#` are special to it.

## Commands

| Command | Does |
|---|---|
| `fs ls [-r] LOCATION` | lists a directory; `-r` lists every key below it |
| `fs stat LOCATION` | shows one object |
| `fs cat LOCATION` | writes an object to standard output |
| `fs put [-type MIME] LOCATION [FILE]` | stores `FILE`, or standard input when `FILE` is absent or `-`; the type is detected from the bytes unless `-type` sets it |
| `fs cp [-r [-dry-run]] SOURCE TARGET` | copies an object; on the disk itself (a server-side copy where the backend has one) when both locations share a DSN, else streamed across backends; `-r` copies every object under a directory (see [Copying a directory](#copying-a-directory)) |
| `fs mv [-r [-dry-run]] SOURCE TARGET` | moves an object; a rename (`Disk.Move`) when both locations share a DSN, else a copy and then a delete of the source; `-r` moves every object under a directory |
| `fs rm [-r] LOCATION` | deletes an object; `-r` deletes everything under a directory |
| `fs url [-expires D] [-put] [-base URL] [-disk NAME] LOCATION` | prints a signed URL, GET by default, valid 15 minutes unless `-expires` says otherwise (at most 7 days); on `file`, `mem`, `sftp`, `ftp` and `webdav` it signs with `FS_SIGNING_SECRET` (see [Signed URLs for local disks](#signed-urls-for-local-disks)) |
| `fs check DSN` | writes a probe object, reads it back, compares and deletes it |

`-json`, before the command, makes every command print JSON. Exit codes: `0` success, `1` failure, `2` usage error.

## A session on local disk

Every transcript on this page is a real run of [`examples/integrations-cli`](../../examples/integrations-cli/README.md), which builds `fs` and drives it from a temporary directory holding `report.txt`, `hello.txt`, `q4.txt` and `summary.txt` (see [Run the session yourself](#run-the-session-yourself)). Lines starting with `$` are the commands; `(exit N)` marks a non-zero exit code.

Store a file, then one from standard input, list the directory with and without `-r`, and read the second back:

```text
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

The SHA-256 is computed while the bytes stream to the disk, so it is the digest of what was stored. Standard input works from a file or a pipe (`printf 'x' | fs put …`); a pipe is written once as a stream. Without `-r`, keys in sub-directories fold into one entry per directory.

Copying to another backend streams the bytes and keeps the content type and digest; moving within one location is a rename, so the file keeps its modification time and no digest is computed:

```text
$ fs cp 'file:srv#reports/q3.txt' 'mem://scratch#q3.txt'
key:          q3.txt
size:         17
modified:     2026-10-08T13:16:59Z
content type: text/plain; charset=utf-8
etag:         837e85b8400398b5ecdd945c87cd4318
visibility:   private
sha256:       522a52306ab093ef4b59cbb0c41a123af96bcb7837d6e68b7d26e875d8a2bf72
$ fs mv 'file:srv#notes/hello.txt' 'file:srv#notes/hi.txt'
key:          notes/hi.txt
size:         17
modified:     2026-10-08T13:16:59Z
content type: text/plain; charset=utf-8
etag:         18dc904ebfecac8f-18dc904ec7f63a8f-11-814dbca
visibility:   private
$ fs -json ls 'file:srv#notes'
[
  {
    "key": "notes/hi.txt",
    "size": 17,
    "mod_time": "2026-10-08T13:16:59.688422543Z",
    "content_type": "text/plain; charset=utf-8",
    "etag": "18dc904ebfecac8f-18dc904ec7f63a8f-11-814dbca",
    "visibility": "private"
  }
]
```

`fs rm -r 'file:srv#notes'` deletes everything under `notes/`, prints nothing and exits 0. A memory location lives only for the command that opens it, so `mem://` is useful as a target to test a copy, not as storage.

## Copying a directory

`-r` makes `cp` and `mv` work on every object under the source directory, each going to the same relative key under the target directory. `-dry-run` prints what would happen and changes nothing:

```text
$ fs cp -r -dry-run 'file:srv#reports' 'file:bak#reports'
would copy reports/2026/q4.txt -> reports/2026/q4.txt
would copy reports/q3.txt -> reports/q3.txt
would copy reports/summary.txt -> reports/summary.txt
$ fs cp -r 'file:srv#reports' 'file:bak#reports'
reports/2026/q4.txt -> reports/2026/q4.txt
reports/q3.txt -> reports/q3.txt
reports/summary.txt -> reports/summary.txt
$ fs mv -r 'file:srv#reports/2026' 'file:srv#archive/2026'
reports/2026/q4.txt -> archive/2026/q4.txt
```

How it behaves:

- **A snapshot.** The source is listed in full before anything is written, so objects the command writes are never listed back, and a move never deletes ahead of the listing.
- **One object at a time, past failures.** Each object is copied or moved on its own; a failure is printed on standard error and the rest carry on. If any failed, the last line is `fs: N of M objects failed` and the exit code is 1.
- **A move never loses a source.** Within one DSN each object is `Disk.Move` (a rename where the backend has one). Across DSNs each object is copied, and its source is deleted only after that copy succeeded, so a failed copy leaves the source where it was. A copy that landed but whose source could not be deleted is reported as such: the object is then in both places.
- **Refusals.** A target inside the source on the same DSN (`fs cp -r 'file:srv#a' 'file:srv#a/copy'`) is refused before anything happens, since the copy would land in the tree being read; so is the root of a disk copied onto that same disk. A source with no objects under it is `not_found`, so a mistyped directory is not a silent success.

Here the target has a file named `moved` where a directory is needed (put there first), so no object can be written, and the move leaves every source in place:

```text
$ fs put 'file:bak#moved' summary.txt
key:          moved
size:         13
modified:     2026-10-08T13:17:00Z
content type: text/plain; charset=utf-8
etag:         18dc904ed6073da5-18dc904ed6746f35-d-814dbf5
visibility:   private
sha256:       e748c0bdd73ba2dacfa3cfe69b8454f3c9a6881d003efce4d58b4e30f862b59c
$ fs mv -r 'file:srv#archive' 'file:bak#moved'
fs: archive/2026/q4.txt -> moved/2026/q4.txt: filesystem: write cli:moved/2026/q4.txt: invalid_argument: a parent of "moved/2026/q4.txt" is a file, so it cannot be created
fs: 1 of 1 objects failed
(exit 1)
$ fs -json mv -r 'file:srv#archive' 'file:bak#moved'
{
  "objects": [],
  "failed": [
    {
      "from": "archive/2026/q4.txt",
      "to": "moved/2026/q4.txt",
      "error": "filesystem: write cli:moved/2026/q4.txt: invalid_argument: a parent of \"moved/2026/q4.txt\" is a file, so it cannot be created",
      "code": "invalid_argument"
    }
  ]
}
(exit 1)
```

With `-json` the report is the whole output: `objects` lists what was transferred (with `-dry-run`, what would be, and `"dry_run": true`), `failed` lists every object that was not, with its code, and `"copied": true` on a move whose copy landed but whose source could not be deleted. No separate error object follows a partial failure; the exit code is still 1.

## A session on S3

The same commands against an S3-compatible endpoint: here an in-process fake S3 ([gofakes3](https://github.com/johannesboyne/gofakes3)) on a random local port, with any credentials (`AWS_ACCESS_KEY_ID=example`, `AWS_SECRET_ACCESS_KEY=example` in the environment). In a shell, keep the DSN in a variable: `B='s3://uploads?region=us-east-1&path_style=true&endpoint=http://127.0.0.1:9000'`, then `fs ls -r "$B#reports"`.

```text
$ fs cp 'file:srv#reports/q3.txt' 's3://uploads?region=us-east-1&path_style=true&endpoint=http://127.0.0.1:63125#reports/q3.txt'
key:          reports/q3.txt
size:         17
modified:     2026-10-08T13:17:00Z
content type: text/plain; charset=utf-8
etag:         837e85b8400398b5ecdd945c87cd4318
visibility:   
sha256:       522a52306ab093ef4b59cbb0c41a123af96bcb7837d6e68b7d26e875d8a2bf72
$ fs ls -r 's3://uploads?region=us-east-1&path_style=true&endpoint=http://127.0.0.1:63125#reports'
  17  2026-10-08 13:17:00  reports/q3.txt
$ fs -json url -put -expires 1m 's3://uploads?region=us-east-1&path_style=true&endpoint=http://127.0.0.1:63125#uploads/new.pdf'
{
  "method": "PUT",
  "url": "http://127.0.0.1:63125/uploads/uploads/new.pdf?X-Amz-Algorithm=AWS4-HMAC-SHA256\u0026X-Amz-Credential=example%2F20261008%2Fus-east-1%2Fs3%2Faws4_request\u0026X-Amz-Date=20261008T131700Z\u0026X-Amz-Expires=60\u0026X-Amz-SignedHeaders=host\u0026x-id=PutObject\u0026X-Amz-Signature=17746dd65a13b8db06a5e05d5596b5c3cb42eb6edd7adb3edc0db0e8eafdcce1",
  "expires_at": "2026-10-08T13:18:00.2107Z"
}
$ fs check 's3://uploads?region=us-east-1&path_style=true&endpoint=http://127.0.0.1:63125'
ok
```

`visibility` is empty because the bucket's policy decides who can read, not a per-object ACL (add `acl=true` for AWS buckets that still use ACLs). `fs url` without `-put` prints a GET URL, valid 15 minutes unless `-expires` says otherwise. `fs check` in a deploy script proves the bucket exists and the credentials can write, read and delete, before the first user upload finds out.

## Signed URLs for local disks

S3, GCS and Azure sign their own URLs. A `file`, `mem`, `sftp`, `ftp` or `webdav` location cannot, so `fs url` signs with the same `signedurl` scheme your application verifies with `fshttp.Signed`, when the environment gives it the secret. Here `fs url` runs first with `FS_SIGNING_SECRET` unset, then with it set and `FS_SIGNING_BASE=https://app.example.com/files`:

```text
$ fs url 'file:srv#notes/hi.txt'
fs: filesystem: presign cli:notes/hi.txt: unsupported: driver *local.Driver has no presign capability (set FS_SIGNING_SECRET and -base or FS_SIGNING_BASE to sign for a backend that cannot sign its own URLs)
(exit 1)
$ fs url -disk media 'file:srv#notes/hi.txt'
https://app.example.com/files/media/notes/hi.txt?exp=1791466320&kid=k1&m=GET&sig=5bf84b77fa41a8ad8e91fe88883642d55f300620c77e0ae464a1bda72c0928f3
```

| Setting | Meaning |
|---|---|
| `FS_SIGNING_SECRET` | the HMAC secret, at least 32 bytes, used as-is. Only an environment variable, never a flag, so it stays out of the process list and shell history |
| `FS_SIGNING_KEY_ID` | the key ID the URL names (`kid`); it must match the ID your verifier holds for that secret. Default `k1` |
| `FS_SIGNING_BASE`, or `-base URL` | the absolute URL where your application mounts `fshttp.Signed` |
| `-disk NAME` | the disk name in the URL path, which must be the name your application serves that location under. Default `cli` |

The URL works only where an application serves that disk with the same secret: `fs` signs, it does not serve. Without the secret, `url` on these schemes is `unsupported` and the message says what to set.

## Errors

Errors print the library's message on standard error and exit 1; with `-json` they print `{"error": …, "code": …}` on standard output, so a script can branch on the code:

```text
$ fs -json stat 'file:srv#reports/missing.txt'
{"error":"filesystem: stat cli:reports/missing.txt: not_found: statat reports/missing.txt: no such file or directory","code":"not_found"}
(exit 1)
$ fs cat 'file:srv#../etc/passwd'
fs: filesystem: read cli:../etc/passwd: invalid_key: key contains a ".." segment
(exit 1)
$ fs put 'file:srv?readonly=true#x.txt' report.txt
fs: filesystem: write cli:x.txt: read_only: disk cli is read-only
(exit 1)
$ fs ls 'file:srv?bucket=x'
fs: filesystem: invalid_argument: disk cli: unknown DSN parameter "bucket" for scheme "file"
(exit 1)
$ fs cp 'file:srv#notes/hi.txt'
usage: fs cp [-r [-dry-run]] SOURCE TARGET
(exit 2)
```

The last is a usage error and exits 2. Every disk `fs` opens is named `cli`, which is the name in its messages.

## Run the session yourself

The transcripts above come from [`examples/integrations-cli`](../../examples/integrations-cli/README.md). It needs only the Go toolchain: it builds `fs` from `cmd/fs` (its own module, so `go build` runs inside it), starts a fake S3 in the same process, and runs every command from a fresh temporary directory.

```sh
cd examples
go run ./integrations-cli
go test ./integrations-cli
```

The test pins every line that does not change between runs (digests, listings, the recursive copy's report, error codes and exit codes); timestamps, ETags, ports and signatures do change. The build step:

<!-- ds:block id=buildfs-auvuhrsk -->
**buildFS** · [`examples/integrations-cli/main.go:100-113`](../../examples/integrations-cli/main.go#L100-L113)

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

And the session, one `step` per command, with only the environment each step names added (the AWS keys for the S3 steps, the signing secret for the signed URL):

<!-- ds:block id=session-9yducmrs -->
**session** · [`examples/integrations-cli/main.go:167-201`](../../examples/integrations-cli/main.go#L167-L201)

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

The same shape works as a smoke test of your own deploy: build `fs` once, then run `fs check` against the production DSN and fail the deploy on a non-zero exit.

## Limits

- **Standard input from a pipe is never retried.** A pipe cannot seek, so it is written once as a stream; a transient failure is reported rather than retried. Pass a file name, or redirect from a file, when that matters.
- **Signed URLs on `file`, `mem`, `sftp`, `ftp` and `webdav` need your application.** `fs` signs with `FS_SIGNING_SECRET`; only an application serving that disk with `fshttp.Signed` and the same secret answers the URL.
- **Different DSNs are different disks.** `cp` and `mv` use the disk's own copy and rename only when both locations have the same DSN, byte for byte; two spellings of one directory (`file:srv` and `file:./srv`) stream the bytes, and `mv` then deletes the source, so a failure between the two leaves both keys.
- **Recursive copies are sequential and not atomic.** `cp -r` and `mv -r` transfer one object at a time; a `mv -r` interrupted part-way leaves the objects already moved at the target and the rest at the source, and the report says which. For millions of objects, or a copy that must verify itself, use the provider's bulk copy or a program built on `filesystem.CopyAcross` ([move to S3](../recipes/move-to-s3.md)).

## Next

- [examples/integrations-cli](../../examples/integrations-cli/README.md): the runnable session behind this page.
- [DSN parameters](../reference/dsn.md): every parameter a location accepts.
- [Configuration](../concepts/configuration.md): the same DSNs from your app's environment.
- [`cmd/fs` reference](../../cmd/fs/README.md): flags, output shapes and exit codes.
