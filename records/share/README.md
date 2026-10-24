# github.com/ubgo/filesystem/records/share

A records plugin for share links: a URL that lets anyone holding it download one file until it expires, is revoked, or has been used a set number of times. The guide is [docs/plugins/share.md](../../docs/plugins/share.md).

## Install

```sh
go get github.com/ubgo/filesystem/records
```

The package is part of the records module, which requires only `github.com/ubgo/sqlb` and the core module.

## Wire it

<!-- ds:block id=newstore-ndhmkyzh -->
**newStore** · [`examples/records-share/main.go:100-110`](../../examples/records-share/main.go#L100-L110)

```go
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager, opts ...share.Option) (*records.Store, *share.Plugin, error) {
	p := share.New(opts...)
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(p))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, p, nil
}
```
<!-- /ds:block hash=54ee41 -->

Mount the link handler with its prefix stripped:

<!-- ds:block id=newmux-6ne3q9m6 -->
**newMux** · [`examples/records-share/main.go:151-156`](../../examples/records-share/main.go#L151-L156)

```go
func newMux(s *records.Store, p *share.Plugin, disks *filesystem.Manager, signer *signedurl.Signer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(sharePrefix, http.StripPrefix(sharePrefix, share.Handler(s, p)))
	mux.Handle(filesPrefix+"/", http.StripPrefix(filesPrefix, fshttp.Signed(disks, signer)))
	return mux
}
```
<!-- /ds:block hash=016e41 -->

The file's disk must be able to sign URLs (S3, GCS and Azure presign; a memory or local disk needs `filesystem.WithURLSigner` and the `fshttp.Signed` handler). A runnable version is [`examples/records-share`](../../examples/records-share).

## Behaviour

- **Tokens.** 32 random bytes, base64url (43 characters), returned once by `Create` in `Share.Token`. Only the hex SHA-256 is stored.
- **Resolving.** The file is loaded first: a trashed or missing file fails without spending anything. Then one conditional `UPDATE` spends a download only when the link is not revoked, not expired and under its limit, so a limit holds under concurrent requests.
- **One error.** Every unusable link (unknown, expired, revoked, exhausted, file trashed or gone) is `CodeNotFound` with the message `share: link not found or no longer valid`.
- **Quarantine.** `Create` refuses a quarantined file (`records.ErrQuarantined`). A link whose file is quarantined later answers the uniform not-found (`Resolve`, `Check`, `ResolveFile`, `CheckFile`, GET and HEAD) and spends nothing; collection listings leave quarantined files out.
- **Purge.** `BeforePurge` deletes a purged file's links in the purge transaction.
- **Handler.** `GET /{token}`: spends one download and answers `302` to `Store.DownloadURL` with a five-minute lifetime, with `Cache-Control: no-store` and `Referrer-Policy: no-referrer`. `HEAD /{token}` checks the link with `Check` and answers `204` without a `Location` and without spending a download (a signed URL handed to a HEAD would be a free download). Unusable link `404`; other failures `500`; other methods `405` with `Allow: GET, HEAD`. It does not rate-limit.
- **Events.** `Create`, `Resolve` and `Revoke` emit `EventCreated` (`share.created`), `EventDownloaded` (`share.downloaded`) and `EventRevoked` (`share.revoked`) through `Store.Emit` inside their transactions, with `FileID` and the link's ID as `Subject`, never the token. `Detail`: `DetailExpiresAt` (RFC 3339, UTC) and `DetailMaxDownloads` on creation, `DetailDownloads` (the count including this one) on a download. `Check`, a refused download, revoking a revoked link and `Prune` emit nothing; a hook's error rolls the change back.
- **Offboarding.** `records.Store.PurgeScope` deletes any link of the scope left over (`ScopePurger`), without events.
- **Pruning.** Expired and revoked links stay (so `Shares` can show them) until their file is purged or `Prune` deletes them; schedule `Prune` with a cutoff of your retention.

## Table

`fs_shares` (constant `TableNameShares`), migrations `share/0001_shares` and `share/0002_shares_access` (columns `password_hash`, `target_kind`, `target_ref`, `mode`, and the table `fs_share_events`, one row per use, deleted with its link):

| Column | Constant | Type | Notes |
|---|---|---|---|
| `id` | `ColID` | key | primary key, `shr_…` |
| `scope` | `ColScope` | key | |
| `file_id` | `ColFileID` | key | index `fs_shares_file` |
| `token_hash` | `ColTokenHash` | short | unique index `fs_shares_token` |
| `expires_at` | `ColExpiresAt` | timestamp | UTC |
| `max_downloads` | `ColMaxDownloads` | 64-bit integer | 0 is unlimited |
| `downloads` | `ColDownloads` | 64-bit integer | |
| `revoked_at` | `ColRevokedAt` | timestamp, nullable | |
| `created_at` | `ColCreatedAt` | timestamp | UTC |

Typed columns: `share.Table`, `share.ID`, `share.Scope`, `share.FileID`, `share.TokenHash`, `share.ExpiresAt`, `share.MaxDownloads`, `share.Downloads`, `share.RevokedAt`, `share.CreatedAt`.

## Options

| Option | What it does | Default |
|---|---|---|
| `share.WithClock(now)` | The plugin's clock, for tests of expiry; the handler reads the Store's clock for file expiry | the Store's clock (`records.WithClock`) |
| `share.WithCollections(c...)` | Collections links may share (folders, owners' roles, your own) | single-file links only |
| `share.WithQuarantinedDeposits()` | Records every upload through a request link (`Deposit`) as quarantined (`records.Quarantine`), unusable until a worker scans it | off: deposits are ready |
| `share.WithTokenBytes(n)` | Longer tokens, `n` random bytes in [`MinTokenBytes` (32), `MaxTokenBytes` (64)]; shorter is refused, since the token is the whole secret of a link without a password (no short codes or vanity slugs). Out of range fails `Migrate` and every link creation with `invalid_argument` | 32 bytes (256 bits) |
| `share.WithMaxLifetime(d)` | Lowers the longest `TTL` ("links last at most 30 days"); `d` in (0, `MaxLifetime`], so the cap can never be raised. Out of range fails as above | `MaxLifetime` (365 days) |

Handler options:

| Option | What it does | Default |
|---|---|---|
| `share.WithUploads(disk, policy)` | Lets request links accept uploads on `disk` under `policy` | request links accept nothing |
| `share.WithUploadPolicy(fn)` | `UploadResolver`: each upload's policy from the link and `WithUploads`' policy (a size per link); validated (invalid is `500`); its error refuses with its status before the file is read | `WithUploads`' policy |
| `share.WithAttemptLimiter(l)` | Counts wrong passwords per link | in process, `DefaultAttempts` per `DefaultAttemptWindow` |
| `share.WithAccessCheck(fn)` | Who may use a live link beyond holding it: `fn(r, link)` runs before the password and before anything is spent; its error refuses with `fshttp.StatusOf` (`permission` `403`, an uncoded error `500`, `not_found` hides the link). Unusable links never reach it | anyone holding the link |
| `share.WithLinkLifetime(d)` | Lifetime of the signed URL a download redirects to, in (0, `MaxLinkLifetime` (1 hour)]; out of range answers `500` to every request | `DefaultLinkLifetime` (5 minutes) |
| `share.WithDeliver(fn)` | `Deliverer`: hands over the file instead of the redirect (a branded download page, a preview, streaming), with the link, the file and the signed URL, after the use is spent | `302` (`303` after a password form) |
| `share.WithErrors(w)` | `fshttp.ErrorWriter` for every failed request: your password page for the `401` (`ErrPassword`), your 404 page, your JSON envelope | plain status text |
| `share.WithUploadDisk(fn)` | Each request-link upload's disk from the request and the link (a tenant's own bucket), before the file is read; its error refuses with its status, an empty or unknown name is `500` | `WithUploads`' disk |
| `share.WithRespond(fn)` | `SuccessWriter` for the JSON answers: a collection's listing (`ListResponse`, an array of `ListedFile`) and a request link's upload (`DepositResponse{ID}`), in your envelope; `SuccessBody` is closed, so a type switch over the two covers it. Each body carries its `records.File` in `File`, never encoded | `JSONSuccess`: the bare array and `{"id": …}` |

## Go API

| Symbol | What it does |
|---|---|
| `New(opts...) *Plugin` | A plugin |
| `(*Plugin).Create(ctx, s, scope, fileID, Options{TTL, MaxDownloads, Password})` | A link to a live file; `TTL` in (0, `MaxLifetime`] (or `WithMaxLifetime`'s lower cap), `MaxDownloads` ≥ 0 (0 unlimited), otherwise `CodeInvalidArgument`; a trashed or unknown file is `CodeNotFound`. The result carries the token |
| `(*Plugin).Resolve(ctx, s, token, WithPassword(pw)...)` | Spends one download and returns the file, or `CodeNotFound` (spending nothing); a link with a password needs `WithPassword`, else `ErrPassword` (`permission`), spending nothing |
| `(*Plugin).CreateFor(ctx, s, scope, Target{Kind, Ref}, Options)` | A link to a collection installed with `WithCollections` (`folders.Plugin.Collection()`, `share.Owners{}` with `OwnerRef(owner, role)`, or your own `Collection`): it lists and serves the collection's live files at each request |
| `(*Plugin).List(ctx, s, token, …)`, `ResolveFile(ctx, s, token, fileID, …)`, `CheckFile` | A collection link's files (up to `MaxListedFiles`), and one of them downloaded (spending one) or checked |
| `(*Plugin).CreateRequest(ctx, s, scope, Target, Options)`, `Deposit(ctx, s, token, disk, body, filename, policy, …)` | A file request link: uploads go into a `Receiver` collection under the policy, with the scope's quota and every hook applying; the body is received before the transaction that records, places and counts it, so a slow uploader holds no database connection; it lists and serves nothing. `MaxDownloads` caps the uploads |
| `(*Plugin).Stats(ctx, s, scope, shareID, recent)` | How often a link was used and its newest uses (time, and the request's actor as viewer) |
| `(*Plugin).Collections(ctx, s, scope, Target)` | The collection and request links of a target |
| `(*Plugin).Check(ctx, s, token)` | Returns the file if the link is usable now, without spending a download; fails as `Resolve` does |
| `(*Plugin).Prune(ctx, s, cutoff)` | Deletes the links, in every scope, that expired or were revoked before `cutoff`; returns how many |
| `(*Plugin).Revoke(ctx, s, scope, shareID)` | Ends a link; twice, or unknown, does nothing |
| `(*Plugin).Shares(ctx, s, scope, fileID)` | A file's links, newest first, without tokens |
| `Handler(s, p, opts...) http.Handler` | Serves file links (`GET`/`HEAD /{token}`), collection links (`GET /{token}` lists as JSON, `GET /{token}/{fileID}` downloads) and request links (`POST /{token}`, multipart `file`, with `WithUploads(disk, policy)`); passwords come as a POSTed `password` field or the `Share-Password` header, wrong passwords counted per link ID by `WithAttemptLimiter` (`Allow` takes an attempt before a check, `Release` gives it back for a right password; default `NewAttemptLimiterClock(DefaultAttempts, DefaultAttemptWindow, s.Now)`, in this process on the Store's clock, at most `MaxTrackedKeys` links; `NewAttemptLimiter(max, window)` is the same on the wall clock), 429 past it. Expired and view-once files follow `records.Store.Viewed`, counted before the download is spent |
| `MaxLifetime` | 365 days, the longest `TTL`, a floor `WithMaxLifetime` can only lower |
| `MinTokenBytes`, `MaxTokenBytes` | 32 and 64, the bounds of `WithTokenBytes` |
| `DefaultLinkLifetime`, `MaxLinkLifetime` | 5 minutes and 1 hour, the default and bound of `WithLinkLifetime` |
| `IDPrefix` | `"shr_"` |
| `PluginName` | `"share"` |

The plugin implements `records.Plugin`, `records.Migrator`, `records.FileHooks` and `records.ScopePurger`. Creating, using and revoking a link emit `share.created`, `share.downloaded` and `share.revoked` (see `EventKindValues`).
