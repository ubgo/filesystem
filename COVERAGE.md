# Coverage

Statement coverage per package, measured on 2026-10-08 (uncommitted work on top of b3fdd80) unless a row says otherwise; every row touched by the three rounds of hard-wire fixes was re-measured on 2026-10-09 (the second and third rounds' after their gaps were closed with tests). Every line still uncovered in a library package is listed under "Known uncovered lines" with the reason no test reaches it; a line number is where the uncovered block starts. "Main entry" means a command's `main` function: its logic lives in a `run` function the tests call.

| Package | Coverage | How it is measured |
|---|---|---|
| `filesystem` (core) | 100% | `task cover` |
| `filesystem/layer`, `local`, `memory`, `sniff`, `signedurl` | 100% | `task cover` |
| `filesystem/upload` | 99.7% | `task cover` (includes the upload sessions and the fetch proxy tunnel); the remaining lines are listed below |
| `filesystem/fshttp` | 99.7% | `task cover`; the remaining line is listed below |
| `filesystem/scan` | 100% | `task cover` (an in-process clamd that follows the documented protocol); `task test:clamav` checks the client against a real clamd |
| `filesystem/filesystemtest` | 99.6% | `task cover`; the remaining lines are listed below |
| `adapters/s3` | 100% | `task cover` (in-process fake S3 plus a fault proxy); `task test:s3` and `task test:rustfs` run the suite on SeaweedFS and RustFS |
| `adapters/gcs` | 99.8% | `task cover` (in-process fake GCS plus a fault proxy); the remaining line is listed below |
| `adapters/azure` | 99.1% (measured 2026-10-09) | `task test:azure` (Azurite in Docker, plus a scripted in-process service for what Azurite does not implement), which writes `.cover/adapters-azure-live.out`; without it the server tests skip (70.5% under `task cover`); the remaining lines are listed below |
| `adapters/sftp` | 100% | `task cover` (in-process SFTP and SSH servers) |
| `adapters/ftp` | 98.9% | `task cover` (an in-process FTP server); the remaining lines are listed below |
| `adapters/sqlblob` | 99.6% | `task cover` (SQLite); `task test:engines` also runs it on Postgres and MySQL; the remaining line is listed below |
| `adapters/webdav` | 99.8% | `task cover` (`golang.org/x/net/webdav` served in-process); the remaining line is listed below |
| `adapters/dropbox`, `onedrive`, `gdrive` | 98.3%, 99.7%, 99.3% | `task cover` (in-process fakes of each HTTP API); the remaining lines are listed below |
| `adapters/vercelblob`, `cloudinary` | 99.2%, 100% | `task cover` (in-process fakes of each HTTP API); the remaining vercelblob lines are listed below |
| `records` and its plugins (`acl`, `audit`, `egress`, `folders`, `locks`, `quota`, `share`, `tags`, `uploadsessions`, `versions`, `webhooks`, `recordshttp`) | 99.9% merged, re-measured 2026-10-09 after round 3's gaps were closed with tests (the lines listed below: share's, webhooks', the shared-lock race and the two fixed-statement errors never; the egress ones in a run where their race fires) | `task cover:records` (SQLite, Postgres, MySQL; each engine covers its own branches, `internal/testdb` and `recordstest` excluded as test infrastructure) |
| `records/uploadsessions` | 100% | `task cover` (SQLite), and in the merged records run |
| `contrib/imaging`, `contrib/imaging/dimensions`, `contrib/imaging/focal`, `contrib/imaging/icc` | 100% | `task cover`; `task test:engines` also runs `contrib/imaging` and dimensions on Postgres and MySQL through `records/recordstest` |
| `contrib/imaging/variants` | 99.7% | `task cover`, and on Postgres and MySQL under `task test:engines`; the remaining line is listed below |
| `contrib/imaging/placeholder` | 96.5% | `task cover`; the remaining lines are listed below |
| `contrib/otel`, `contrib/keynorm`, `contrib/cloudflare`, `contrib/notify` | 100% | `task cover` |
| `contrib/notify/eventgrid`, `pubsub`, `sqs` | 100% | `task cover` (in-process fakes of each service; Pub/Sub through Google's `pstest`) |
| `contrib/cloudfront` | 100% | `task cover` |
| `contrib/archive`, `contrib/crypt` | 100%, 98.3% | `task cover`; the remaining crypt lines are listed below |
| `contrib/remote`, `contrib/pickers` | 98.8% (measured 2026-10-09), 99.5% | `task cover` (in-process HTTP servers); the remaining lines are listed below |
| `contrib/imaging/webp`, `avif`, `heic` | 100% | `task test`, which runs them twice: with the system library allowed and with `-tags nodynamic` |
| `contrib/video`, `contrib/audio` | 99.3% (measured 2026-10-09, with the HLS ladder), 100% | `task cover` (ffmpeg and ffprobe on the machine, plus fake programs for the failure cases); the remaining video line is listed below |
| `contrib/video/ffmpeg`, `contrib/pdf` | 98.3%, 99.8% | `task cover` (ffmpeg, ffprobe and Poppler on the machine, plus fake programs for the failure cases); the remaining lines are listed below |
| `contrib/videoplatform` (`cfstream`, `mux`, `internal/rest`) | 99.1% (98.4%, 98.6%, 100%) | `task cover` (in-process fakes of each API); the remaining lines are listed below |
| `contrib/office`, `contrib/ocr` | 97.7%, 99.4% | `task cover` (fake `soffice` and `tesseract` scripts, plus the real programs when installed; their tests skip otherwise); the remaining lines are listed below |
| `contrib/vision`, `contrib/vision/google` | 100%, 98.2% | `task cover` (an in-process fake of the API) |
| `contrib/svg`, `contrib/svg/raster` | 100% | `task cover` |
| `contrib/gin`, `echo`, `fiber`, `chi`, `gqlgen` | 100% | `task cover` (each drives the shared fake app in `contrib/internal/fwtest` over a real server) |
| `contrib/ent`, `contrib/gorm`, `contrib/sqlc` | 100%, 100%, 97.1% (measured 2026-10-09; the one `sqlc` line is listed below) | `task cover` on SQLite; `task test:engines` also runs them on Postgres and MySQL; ent and sqlc run their real code generators |
| `jobs`, `jobs/diskjobs` | 98.9%, 100% | `task cover`, which measures the jobs module with `-coverpkg=./...` because the scheduler and the database-free jobs moved to `diskjobs` while their tests stay in `jobs` (through its aliases); the same three `jobs` lines as before remain, now a larger share of a smaller package; they are listed below |
| `jobs/river` | 100% | `go test -cover` with `FILESYSTEM_RIVER_PG_DSN` set to the host Postgres (what `task test:engines` sets); 89.2% under `task cover`, without it (the uncovered lines are the periodic job's constructor and `Enqueue`/`EnqueueWith`) |
| `jobs/asynq` | 94.8% under `task cover` | without Redis `Enqueue` and `EnqueueWith` do not run; `task test:asynq` runs them against a throwaway Redis (97.2% measured that way on 2026-10-08, not re-measured); the `Scheduler.Register` error once listed below is now reached by a cronspec `Spec` that does not parse |
| `contrib/tus` | 98.3% | `task cover`; the remaining lines are listed below |
| `cmd/fs` | 99.1% | `task cover`; the rest is the main entry |
| `internal/mdwrap`, `internal/covmerge` | 76.6%, 72.1% | the rest is the main entry |
| `examples/*` | 32% to 91% (basics 71%, drive 77%, mediaserver 60%; the framework examples about 33%) | the rest is the main entry and the wiring's error returns |

The records module and its plugins are also swept for faults: `internal/testdb.OpenFaulty` wraps the engine's driver so a test can fail its Nth database step, and each package's `TestFaultSweep` fails every step of every operation in turn, on all three engines, requiring the injected error and unchanged rows (and objects) each time. `TestMigrateFaultSweep` does the same for a first `Migrate` and then requires the next run to complete the schema.

## Known uncovered lines

| Where | Why it is not reached |
|---|---|
| `filesystemtest/cases.go:179` (write-report failure message) | It reports a violation of a guarantee the Disk computes itself (size and SHA-256), so no driver can trigger it |
| `filesystemtest/suite.go:85` (`t.Error`) | Runs only when a case fails, which a passing test run never has |
| `filesystemtest/cases_ops.go:247` (`SetStorageClass("")` wrong code) | The Disk refuses an empty storage class before any driver sees it, so no driver can give it another code |
| `fshttp/sendfile.go:76` (`DriverKey` error) | The entry's key comes from the Disk's own `Stat`, so it is always a valid key |
| `adapters/azure/opener.go:150`, `:171`, `:251` (client construction error, shared key, SAS and Entra ID); `adapters/azure/account.go:61` (the `AccountName` delegation client) | The Azure SDK never validates the endpoint when building a client |
| `adapters/azure/delegate.go:145` (SAS signing error) | The SDK's signers fail only for a nil credential, a zero expiry or an empty or malformed permission string, and `DelegatePrefix` refuses a missing credential and always sets a future expiry and permissions built by `sas.ContainerPermissions`; the HMAC write behind the signature never fails |
| `adapters/azure/url.go:106` (`ParseAllowedIP` error) | `AllowedIP` is validated by the Disk before the driver signs, so the parse never fails here |
| `adapters/azure/multipart.go:127`, `:227` (the upload's options refused at a part or at completion) | They are the options `CreateMultipart` stored after checking them with the same function, so they cannot fail later |
| `adapters/gcs/delegate.go:127` (`downscope.NewTokenSource` error) | It fails only for a nil root source or an empty or incomplete rule, and the driver always passes its root and one complete rule |
| `adapters/ftp/ftp.go:526` (`Read` cancelled between `RETR` and handing back the body) | Needs the context to be cancelled in the instant between the server accepting `RETR` and the stop check, a timing race the in-process server cannot produce deterministically |
| `adapters/ftp/ftp.go:761` (`refusedAsExisting` for an error that is not a 550 or 553 reply) | The in-process server refuses a rename only with 550; a rename failing another way needs a server that answers another code |
| `adapters/ftp/ftp.go:901` (`serverKey` skipping an empty name, `.`, `..` or a name with a slash) | The in-process server never lists such names; the guard is for servers that do |
| `adapters/ftp/ftp.go:739` (`replace` returning a failure below the protocol after the old object was put back) | A failure that is not a reply means the control connection broke, so the rollback rename on that same connection cannot succeed and the result is a `*StrandedError` instead; the branch keeps such a failure from being reported as a refusal if a client library ever recovers the connection. The neighbouring cut during the move aside is covered by `TestReplaceCutWhileMovingAside` |
| `adapters/sqlblob/sqlblob.go:255` (`Write` encoding `WriteOptions.Metadata`) | `json.Marshal` of a `map[string]string`, which cannot fail (string keys and values always encode; invalid UTF-8 is replaced, not refused); the branch stays so a future change of the metadata type cannot store a half-encoded column. Every statement's `Build()` error now goes through one helper (`stmt.go` `build`) and is covered |
| `adapters/dropbox/dropbox.go:145`, `:354`, `:399`, `:407`; `onedrive/onedrive.go:352`; `gdrive/gdrive.go:392`, `:666`; `vercelblob/vercelblob.go:482` | `json.Marshal` of fixed request types (strings, numbers, booleans, and dropbox's `Dropbox-API-Path-Root` map of strings), which cannot fail |
| `adapters/dropbox/dropbox.go:416`, `:924`; `gdrive/gdrive.go:366`; `vercelblob/vercelblob.go:296`; `webdav/webdav.go:352` | `http.NewRequest` or `url.Parse` on a URL built from an endpoint already validated when the disk was opened |
| `contrib/crypt/crypt.go:144`, `:149`; `keyring.go:153` | `crypto/rand` reads, which do not fail on supported platforms |
| `contrib/crypt/crypt.go:162`; `format.go:157`; `keyring.go:113`, `:117` | `aes.NewCipher` and `cipher.NewGCM` on keys whose length is checked first, which cannot fail |
| `contrib/imaging/placeholder/placeholder.go:196`, `:201`, `:255`, `:284`; `handler.go:92` (embedded font fails to parse, `opentype.NewFace` error, PNG encode into a buffer, and the handler's 500 after them) | The font is compiled into the binary and the options are constants, and encoding into a `bytes.Buffer` cannot fail, so none of these can happen at run time |
| `contrib/pickers/sources.go:234`; `contrib/remote/preview.go:99`; `contrib/vision/google/google.go:160` | `json.Marshal` of fixed request types, which cannot fail |
| `contrib/remote/preview.go:175`, `proxy.go:92`; `contrib/remote/oembed.go:191`, `:205` (an allowlisted endpoint, or the page's final URL, that does not parse) | `http.NewRequest` or `url.Parse` on a URL that passed the SSRF guard's own parse just before |
| `contrib/videoplatform/plugin.go:145` | Migration DDL fixed in code, built by `sqlb`, which refuses only a malformed builder |
| `contrib/videoplatform/plugin.go:337` (the `return nil` after the status switch) | The switch takes ready, errored and deleted; the only other status, preparing, never gets there: `admit` refuses it after any other status, preparing after preparing returns earlier as unchanged, and `clean` turns an unknown status into errored |
| `contrib/office/office.go:188`, `:192`, `:215`; `contrib/ocr/ocr.go:194` | File operations inside a private temporary directory the function just created (write, close, read back), which do not fail on a working filesystem |
| `contrib/video/ffmpeg/ffmpeg.go:156` (`Stat` of the file just opened) | Stat on an open descriptor of a file this function created does not fail on a working filesystem |
| `contrib/video/ffmpeg/ffmpeg.go:286` (the final `flush` error in `Measure`) | `packetSpan.Write` already refuses a held line over `maxPacketLine`, so the last line can never be too long when it is flushed |
| `contrib/video/owned.go:190`, `contrib/pdf/owned.go:142`, `contrib/ocr/owned.go:142` (a derived object copied to another disk at a size other than its own) | `CopyAcross` writes with the size of its own `Stat` as `SizeHint`, and `Disk.Write` refuses a body of any other length (`invalid_argument`), so the sizes differ only when the object is replaced between the plugin's `Stat` and `CopyAcross`'s, a race |
| `contrib/tus/tus.go:225` (handler construction) | tusd accepts every configuration this package builds |
| `jobs/export.go:218`, `:223`, `:249` (`zip.Writer` `Create` of the manifest, encoding it, `CreateHeader` of a file) | `zip.Writer` buffers its output and the headers are always valid, so a failing destination surfaces at the file copy or `Close` before these calls can see it |
| `jobs/river/river.go` (the periodic job's constructor, `Enqueue`) without `FILESYSTEM_RIVER_PG_DSN` | Only a running River client calls them; with the DSN set, `TestOnPostgres` covers both |
| `upload/proxy.go:53` (`SplitHostPort` of the address the transport dials) | `net/http` always dials `host:port`, so the split never fails |
| `upload/proxy.go:108` (a name that resolves to no address) | The resolver answers a name with no address with an error, which the line before returns; an empty answer without one does not happen |
| `upload/proxy.go:123` (a context deadline earlier than the dial timeout) | `net/http` dials with a context of its own that carries no deadline (so the CONNECT exchange is bounded by `fetchDialTimeout`); the branch keeps an earlier deadline should a caller's context reach the dial |
| `upload/proxy.go:133` (writing the CONNECT request fails) | The request is written to a connection dialed a moment before; a peer that hangs up first fails the read of the answer instead, which is tested |
| `adapters/azure/opener.go:171` | As `:150`: the SDK builds a client for a SAS URL without validating it |
| `contrib/imaging/variants/handler.go:112` (the variant's disk is not in the Manager) | A variant is written to a disk of the Store's Manager and its row names that disk, so the lookup fails only if the application removed the disk from the Manager between the write and the read |
| `contrib/video/hls.go:162`, `:214`, `:232` (`os.MkdirTemp` in `TempDir`, opening a file just listed, listing the directory just made) | `withWorkFile` has already made a file in the same `TempDir`, and the encode's own directory is read only by this function; an error here needs the filesystem to fail between two calls |
| `contrib/video/owned.go:237` (the ladder's disk is not in the Manager after the commit) | The disk name comes from the row being purged or replaced, which names a disk of the Store's Manager; a Manager is fixed when the Store is built |
| `contrib/sqlc/sqlc.go:139` (`fs.ReadFile` in `FSWith`) | It reads the files embedded in the binary, one by one as `fs.WalkDir` lists them; an embedded file always reads |
| `contrib/tus/tus.go:255` (`GetInfo` of an upload just loaded) | tusd's file store reads the info when it loads the upload, so `GetInfo` on it returns that copy and does not fail |
| `contrib/tus/tus.go:267` (an upload path with more than one segment) | The handler is mounted with its base path stripped, as `New` documents, so the path is the upload ID alone; the line keeps a handler mounted another way from reading the wrong segment |
| `contrib/videoplatform/cfstream/cfstream.go:147`, `mux/mux.go:166` (`rest.Merge` error) | `Merge` fails only for a base that does not marshal to a JSON object (tested in `internal/rest`); both callers pass fixed request structs |
| `records/webhooks/secrets.go:179` (the conditional update of the secrets matches no row) | Another rotation would have to change the row between the read and the update of one transaction; SQLite serialises the transaction, and on Postgres and MySQL the race needs two transactions timed into that gap |
| `records/egress/egress.go:218`, `subjects.go:120` (the second charge after a unique violation, per scope and per subject) | Reached only when two first reads of a day race on the INSERT; `TestConcurrentReadsAllCounted` provokes the race, but whether it happens in a given run depends on scheduling, so the merged profile has it in some runs and not others |
| `records/locks/shared.go:64` (a unique violation inserting a first shared lock) | Two first shared locks of one owner on one file must race between the read and the insert of one transaction that holds the file's row lock; the row lock serialises them on every engine, so the second sees the first's row |
| `records/locks/shared.go:120` (`forUpdate` error) | It renders a fixed statement built by `sqlb`, which refuses only a malformed builder |
| `records/migrate.go:205` (`ledgerDDL` error) | The ledger's DDL is fixed in code and built by `sqlb` for the Store's own engine, which refuses only a malformed builder |
| `records/names.go:122` (`checkNames` without a mapping) | Its one caller, `renamed`, returns before calling it when the Store has no mapping; the guard keeps a future caller from calling a nil mapping |
| `records/share/password.go:79` (`pbkdf2.Key` error) | `crypto/pbkdf2` fails only for a key length it cannot produce or, in FIPS mode, a too-short salt or key, and the iteration count, salt and key lengths are constants within its limits |
