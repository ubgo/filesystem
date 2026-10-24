# github.com/ubgo/filesystem/records/uploadsessions

The records-backed `upload.SessionStore`: upload sessions (server-issued upload tokens valid for a window, UP-15) kept in the database, so they survive a restart and are shared by every instance behind a load balancer. `upload.Sessions` does every check (the secret in constant time, the expiry by its own clock, the limits against the endpoint's policy); this package only keeps the rows. The guide is [Upload sessions](../../docs/plugins/upload-sessions.md); the concept is introduced in [Uploads](../../docs/concepts/uploads.md#upload-sessions-a-token-valid-for-a-window).

## Install

```sh
go get github.com/ubgo/filesystem/records
```

The package is in the records module, which requires only `github.com/ubgo/sqlb` and the core module.

## Wire it

```go
p := uploadsessions.New()
store, err := records.New(db, sqlb.PostgresDialect{}, disks, records.WithPlugins(p))
// store.Migrate(ctx) creates fs_upload_sessions.
sessions, err := upload.NewSessions(p.Store(store), upload.SessionsOptions{})
h := upload.Handler(disk, policy, upload.HandlerOptions{Resolve: sessions.Resolver()})
```

Schedule `sessions.Sweep(ctx)`; an expired session is refused whether or not it was swept.

## Go API

| Name | What it is |
|---|---|
| `New()` | the plugin; install it with `records.WithPlugins` |
| `Plugin.Store(s)` | the `upload.SessionStore` over `s`'s database; statements join the transaction in `ctx` |
| `Store.SaveSession`, `LoadSession`, `DeleteSession`, `SweepSessions` | the four `upload.SessionStore` methods: a duplicate ID is `exists`, an unknown one `not_found`, delete is idempotent, sweep is one `DELETE` of the sessions expired at `now` |

## Table

`fs_upload_sessions` (constant `TableNameSessions`), migration `uploadsessions/0001_upload_sessions`:

| Column | Constant | Type | Notes |
|---|---|---|---|
| `id` | `ColID` | key | primary key, `ups_…` |
| `secret_hash` | `ColSecretHash` | short | hex SHA-256 of the token's secret; never the secret |
| `storage_key` | `ColKey` | long | the session's one key, or `""` |
| `prefix` | `ColPrefix` | long | the session's key prefix, or `""` |
| `max_size` | `ColMaxSize` | 64-bit integer | 0 keeps the policy's |
| `allowed` | `ColAllowed` | doc | JSON array of type patterns; a row that does not parse is an error, never a session without type limits |
| `created_at` | `ColCreatedAt` | timestamp | UTC |
| `expires_at` | `ColExpiresAt` | timestamp | UTC; index `fs_upload_sessions_expires` for the sweep |

Sessions belong to no scope, so `PurgeScope` leaves them; they expire within `upload.MaxSessionLifetime` (7 days) and the sweep deletes them.
