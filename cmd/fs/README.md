# fs

`github.com/ubgo/filesystem/cmd/fs` is a command-line tool for any disk the library opens: list, inspect, read, write, copy across backends, delete, sign URLs and check a disk. It is its own module, so the core never depends on the cloud SDKs it uses. The guide with full sessions is [docs/integrations/cli.md](../../docs/integrations/cli.md); this page is the reference.

## Build

```sh
cd cmd/fs
go build -o fs .
```

The module's `go.mod` replaces `github.com/ubgo/filesystem` and the `s3`, `gcs`, `azure` and `sftp` adapters with the working tree, so it builds from a clone; `go install …@latest` refuses modules with `replace` directives.

## Usage

```
fs: work with any disk ubgo/filesystem can open.

Usage: fs [-json] COMMAND ARGS

A LOCATION is a DSN whose fragment is the key: 'file:///srv/files#docs/a.pdf'.

Commands:
  ls [-r] LOCATION                                           list a directory (recursively with -r)
  stat LOCATION                                              show one object
  cat LOCATION                                               write an object to standard output
  put [-type MIME] LOCATION [FILE]                           store FILE (or standard input) at LOCATION
  cp [-r [-dry-run]] SOURCE TARGET                           copy an object (every object under a directory with -r), on the disk itself when both share a DSN
  mv [-r [-dry-run]] SOURCE TARGET                           move an object (every object under a directory with -r), a rename when both share a DSN
  rm [-r] LOCATION                                           delete an object, or everything under a directory with -r
  sync [-delete] [-dry-run] [-checksum] SOURCE TARGET        copy new and changed objects from one directory to another (and delete what the source lacks with -delete)
  sweep -older D [-dry-run] LOCATION                         delete objects under a directory older than D (never the root)
  url [-expires D] [-put] [-base URL] [-disk NAME] LOCATION  print a signed URL (default 15m, GET)
  check DSN                                                  write, read and delete a probe object

Schemes: file, mem, s3, gs, azblob, sftp, ftp, ftps, ftps-implicit, webdav, webdavs, cloudinary, vercelblob. url on file, mem, sftp, ftp and webdav signs with FS_SIGNING_SECRET (and FS_SIGNING_KEY_ID, FS_SIGNING_BASE).

Examples:
  fs ls -r 'file:///srv/files#avatars'
  fs cp 'file:///srv/files#a.png' 's3://media?region=auto&endpoint=https://acct.r2.cloudflarestorage.com#a.png'
  fs -json stat 's3://media?region=us-east-1#reports/q3.pdf'
```

`fs` with no command prints this to standard output and exits 2; `fs help` prints it and exits 0.

## Locations

A location is a DSN whose fragment (after `#`) is the key, or the directory for `ls` and `rm -r`. The schemes registered are `file` (`local.Opener`), `mem` (`memory.Opener`), `s3` (`s3.Opener`), `gs` (`gcs.Opener`), `azblob` (`azure.Opener`), `sftp` (`sftp.Opener`), `ftp`, `ftps` and `ftps-implicit` (`ftp.Opener`), `webdav` and `webdavs` (`webdav.Opener`), `cloudinary` (`cloudinary.Opener`) and `vercelblob` (`vercelblob.Opener`). Every adapter parameter and the Mux's own (`prefix`, `public_url`, `visibility`, `readonly`) are accepted: [DSN parameters](../../docs/reference/dsn.md). Each location opens its own disk, named `cli`, except that `cp` and `mv` between two locations with the same DSN open it once.

## Commands and flags

| Command | Arguments | Flags | Library call |
|---|---|---|---|
| `ls` | `LOCATION` | `-r` recursive | `Disk.List` |
| `stat` | `LOCATION` | | `Disk.Stat` |
| `cat` | `LOCATION` | | `Disk.Read`, copied to standard output |
| `put` | `LOCATION [FILE]` (`-` or absent: standard input) | `-type MIME` (empty: detected from the bytes) | `Disk.Write` |
| `cp` | `SOURCE TARGET` | `-r` every object under the source directory, to the same relative key under the target directory; `-dry-run` (with `-r`) print what would be copied and change nothing | `Disk.Copy` when both have the same DSN, else `filesystem.CopyAcross`; per object with `-r` |
| `mv` | `SOURCE TARGET` | `-r` and `-dry-run`, as for `cp` | `Disk.Move` when both have the same DSN (a rename where the backend has one), else `filesystem.MoveAcross`; with `-r`, per object, and across DSNs `CopyAcross` then `Disk.Delete` of that object only once its copy succeeded |
| `rm` | `LOCATION` | `-r` everything under a directory | `Disk.Delete`, or `Disk.DeletePrefix` with `-r` |
| `sync` | `SOURCE TARGET` (directories) | `-delete` also delete target objects the source lacks, only inside the target directory; `-dry-run` print every planned copy and delete and change nothing; `-checksum` compare objects of equal size by hashing both (for backends whose ETags cannot be compared, such as `file`) | `filesystem.Sync`; one disk when both have the same DSN, where overlapping directories are refused |
| `sweep` | `LOCATION` (a directory, never the root) | `-older D` (required) minimum age; `-dry-run` list what would be deleted | `upload.Sweep`, or `upload.FindStale` with `-dry-run` |
| `url` | `LOCATION` | `-expires D` (default `15m`), `-put` sign an upload, `-base URL` where the application serves signed URLs (default `$FS_SIGNING_BASE`), `-disk NAME` the disk name in the URL (default `cli`) | `Disk.SignedURL`, with a `signedurl` signer when `FS_SIGNING_SECRET` and a base are set |
| `check` | `DSN` | | `Disk.Check` |

The global flag `-json` comes before the command.

## Output

Text output for one object (`stat`, `put`, `cp`, `mv`):

```
key:          reports/q3.txt
size:         18
modified:     2026-10-03T09:33:12Z
content type: text/plain; charset=utf-8
etag:         18dafb318f51b04d-12
visibility:   private
sha256:       4c694ad7a5ea27610e73d5dca732d67b51100682543877a8a882584667371a9d
```

`cp -r` and `mv -r` print one line per object transferred, `SOURCE-KEY -> TARGET-KEY` (`would copy …` or `would move …` with `-dry-run`), and one line per failure on standard error, then `fs: N of M objects failed` and exit 1 if any failed.

`sync` prints one line per action, `copy KEY -> TARGET` or `delete KEY -> KEY` (`would …` with `-dry-run`), failures on standard error, then `N copied, N deleted, N unchanged, N failed`, and exits 1 if any failed. `sweep` prints `N deleted`, or `would delete KEY` per object with `-dry-run`.

`sha256` appears only when the command streamed the bytes (`put`, a `cp` or `mv` between two DSNs); `stat` has none, because no built-in driver stores a digest. `ls` prints size, modification time and key per object, and `-  -  dir/` per folded directory. `url` prints the URL alone; `check` prints `ok`; `rm` prints nothing.

With `-json`:

| Output | Shape |
|---|---|
| an object | `{"key", "dir" (omitted when false), "size", "mod_time" (omitted when zero), "content_type", "etag", "sha256", "visibility", "metadata"}`, empty strings and maps omitted |
| `ls` | an array of objects, `[]` when empty |
| `url` | `{"method", "url", "headers" (omitted when none), "expires_at"}` |
| `cp -r`, `mv -r` | `{"dry_run" (omitted when false), "objects": [{"from", "to"}], "failed": [{"from", "to", "copied" (omitted when false), "error", "code"}]}`; the whole report, so a partial failure prints no separate error object. `copied` is true when `mv` wrote the target but could not delete the source |
| `sync` | `{"dry_run" (omitted when false), "copied", "deleted", "unchanged", "failed", "items": [{"action", "key", "target", "size", "error", "code"}]}`, `error` and `code` only on a failed item; the whole report, as for `cp -r` |
| `sweep` | `{"dry_run" (omitted when false), "count", "keys" (with `-dry-run` only)}` |
| `rm`, `check` | `{"ok": true}` |
| any failure | `{"error": "<message>", "code": "<filesystem.Code>"}` on standard output |

## Exit codes

| Code | Meaning |
|---|---|
| `0` | success |
| `1` | the operation failed; the message is on standard error, or the JSON error on standard output with `-json` |
| `2` | usage error: unknown command, wrong number of arguments, bad flag |

## Environment

S3 credentials and region defaults come from the AWS default chain (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_PROFILE`, shared config files, instance roles); GCS from Application Default Credentials; Azure from the connection string in `AZURE_STORAGE_CONNECTION_STRING` (or the variable `connection_env` names); SFTP from a key file (`identity`) or the variable `password_env` names. A DSN never carries a secret.

| Variable | Used by | Meaning |
|---|---|---|
| `FS_SIGNING_SECRET` | `url` | HMAC secret (at least 32 bytes, used as-is) for backends that cannot sign their own URLs (`file`, `mem`, `sftp`); the same bytes the application gives `signedurl.Key.Secret`. Read only from the environment, never from a flag, so it stays out of the process list |
| `FS_SIGNING_KEY_ID` | `url` | the key ID the URL names; must match the application's verifier (default `k1`) |
| `FS_SIGNING_BASE` | `url` | default for `-base`: the absolute URL where the application mounts `fshttp.Signed` |

The URL is `<base>/<disk>/<key>?…`, so `-disk` must be the name the application serves the location under. Backends that sign their own URLs (S3, GCS, Azure) ignore these variables. Interrupt (Ctrl-C) cancels the running operation through its context.

## Limits

- `cp -r` and `mv -r` transfer one object at a time, in the listing's order, with no parallelism; for millions of objects use the provider's bulk copy.
- `mv -r` is not atomic: each object is moved on its own, and a failure leaves the objects already moved at the target and the rest at the source (the report lists which).
- Standard input from a pipe is written once as a stream; it is never retried after a transient failure.
