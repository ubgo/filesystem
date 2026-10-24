# filesystemtest

`github.com/ubgo/filesystem/filesystemtest` holds the conformance suite every driver must pass and a `Fake` disk with assertions for application tests.

How to test an application with it is in [Testing](../docs/start/testing.md); how to run the suite against a driver you write is in [Write a driver](../docs/extending/write-a-driver.md).

## Application tests

| Name | What it is |
|---|---|
| `NewFake(tb, name, options...)` | an empty in-memory disk named `name`, configured with Disk options like any other; fails the test at once if the options are invalid |
| `Fake` | embeds `*filesystem.Disk`, so it is handed to the code under test wherever a disk is expected |
| `Fake.AssertExists(key)` | fails the test unless the key exists |
| `Fake.AssertMissing(key)` | fails the test if the key exists |
| `Fake.AssertContent(key, want)` | fails the test unless the key holds exactly `want` |
| `Fake.AssertCount(dir, n)` | fails the test unless `dir` holds exactly `n` objects, counted recursively |
| `Fake.AssertEmpty()` | fails the test unless the whole disk is empty |

The fake is built on the memory driver, which has nearly every capability. Code that relies on a capability your production backend lacks (metadata, headers, per-object visibility) passes against the fake and fails in production with `CodeUnsupported`; see [Capabilities](../docs/concepts/capabilities.md).

## Upload test builders

| Name | What it is |
|---|---|
| `NewUploadRequest(tb, target, fields, files...)` | a `multipart/form-data` POST to `target` with the form fields (in key order) then the files, as a browser sends it, using `tb`'s context; hand it to an upload handler's `ServeHTTP` |
| `UploadFile` | one file part: `Field` (upload handlers read `"file"` by default), `Name`, `Content` |
| `FakeImage(tb, format, width, height)` | a real, decodable `PNG`, `JPEG` or `GIF` (`ImageFormat`) of those dimensions; fails the test for an unknown format or a side outside `1..MaxFakeImageSide` (8192) |
| `FakeBytes(size)` | `size` deterministic bytes detected as `application/octet-stream`, for size-limit tests |
| `PadTo(tb, data, size)` | `data` followed by zero bytes up to exactly `size`; an image padded this way still decodes and is detected as its type, since decoders stop at their end marker; fails the test when `data` is longer |

## Conformance suite

| Name | What it is |
|---|---|
| `Run(t, factory)` | runs every case as a sub-test; a case whose capability the disk does not declare is skipped |
| `Factory` | `func(t *testing.T) *filesystem.Disk`: returns a new, empty disk for one case; use `t.Cleanup` for teardown |
| `Cases()` | the cases, for running a subset |
| `Case` | `Name`, `Needs` (whether the case applies to a disk's capabilities; nil means always) and `Check` (returns the first violation, or nil) |

Run the suite against a bucket, container or directory nothing else uses while it runs. The `SweepAbandoned` cases sweep the whole backend, as the capability does, with a cutoff an hour in the future, so they abort any other client's multipart uploads in progress and remove its staging files; their assertions concern only what they created themselves.

The suite runs against a `*filesystem.Disk`, not a bare driver, so it checks what applications see: the driver's contract and the Disk's portable logic together. Its own tests feed it deliberately broken drivers and require every case to notice.

| Always | When declared |
|---|---|
| `WriteThenRead`, `OverwriteReplaces`, `EmptyObject`, `LargeObject`, `WriteReportsSizeAndHash`, `StatMissingIsNotFound`, `ReadMissingIsNotFound`, `StatReportsSizeAndTime`, `Exists`, `DeleteRemoves`, `DeleteMissingSucceeds`, `ReadRanges`, `UnicodeAndSpacesInKeys`, `FailingBodyStoresNothing`, `MaxSizeStoresNothing`, `DetectsContentType`, `ListRecursive`, `ListFoldsDirectories`, `ListPaginates`, `ListEmptyDirectory`, `ListDoesNotMatchSiblingPrefix`, `CopyDuplicates`, `CopyMissingIsNotFound`, `MoveRenames`, `DeletePrefixScoped`, `DeleteKeys`, `ConcurrentWrites`, `ConditionalRead` (`ReadIf` answers not modified for a matching ETag or time, the body otherwise), `LetterCase` (`A.txt` and `a.txt` are two objects unless the disk declares `CaseInsensitive`), `KeyCharactersRoundTrip` (a key holding `: * ? < > \| " % #`, a space and an accent writes, reads and lists unchanged), `DirExists` (a directory exists while a key is under it, an object's key is not a directory, the root always exists, and `DeletePrefix` removes it) | `StoresContentType` (`ContentType`), `StoresMetadata` (`Metadata`), `StoresHeaders` (`Headers`), `ConditionalWrite` (`ConditionalWrite`), `Visibility` (`SetVisibility`), `Multipart` and `MultipartAbort` (`Multipart`), `SweepAbandoned` (`SweepAbandoned` and `Multipart`), `SweepAbandonedKeepsObjects` (`SweepAbandoned` without `Multipart`, such as SFTP: the sweep succeeds, keeps stored objects and leaves the disk writable), `Append` (`Append`: concatenates, creates a missing key, a failing body appends nothing, concurrent appends never interleave), `CompareAndSwap` (`CompareAndSwap`: a stale ETag is `conflict`, and of two racing writers exactly one wins), `SetModTime` (`SetModTime`), `Tags` (`Tags`), `MakeDir` (`Dirs`: an empty directory exists, lists no keys, takes keys, cannot replace an object, and goes with `DeletePrefix`), `MakeDirWithoutDirsWritesNothing` (without `Dirs`: `MakeDir` stores no marker), `FindAbandoned` (`FindAbandoned` and `Multipart`: the preview reports the upload with its key under the sweep's cutoff rule, removes nothing, repeats, and no longer reports it once swept), `FindAbandonedKeepsObjects` (`FindAbandoned` without `Multipart`: only staging files, no stored object, nothing removed); `AppendUnsupportedIsRefused`, `CompareAndSwapUnsupportedIsRefused`, `MetadataUnsupportedIsRefused`, `HeadersUnsupportedIsRefused`, `ConditionalWriteUnsupportedIsRefused`, `SweepAbandonedUnsupportedIsRefused` and `FindAbandonedUnsupportedIsRefused` run when the capability is not declared |

The copy, move and delete cases run on every disk because the Disk's fallbacks must give the same result as a native implementation. Every built-in driver passes the suite: local and memory in `task test`, S3 against an in-process fake and against SeaweedFS (`task test:s3`), GCS against an in-process fake, Azure against Azurite (`task test:azure`), SFTP against an in-process server.
