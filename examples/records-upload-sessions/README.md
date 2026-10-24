# Upload sessions in the database

Upload sessions kept by the `records/uploadsessions` plugin: the app's API issues a token for one key prefix with limits no wider than the endpoint's policy, and a client holding only that token uploads with it. The sessions live in the records database, so every instance behind a load balancer, and the app after a restart, redeems the same tokens. It runs on SQLite over an in-memory disk, with a clock the demo moves past the expiry. The guide [Upload sessions](../../docs/plugins/upload-sessions.md) explains each step.

## Run it

```sh
cd examples
go run ./records-upload-sessions
go test ./records-upload-sessions
```

It needs no network or environment variables.

## What it does

```text
issued ups_… for prefix users/alice until 09:15
with the token:                201 users/alice/…
no token:                      403
forged secret:                 403
over the session's 64 bytes:   413
a PNG the endpoint allows:     415
another instance:              201 users/alice/…
after the expiry:              403
sweep deleted 1 expired session(s)
revoked:                       403
issue wider than the endpoint: invalid_argument
```

1. The app issues a token for `users/alice`, at most 64 bytes of plain text, for 15 minutes.
2. An upload with the token is stored under the prefix. Without it, or with a forged secret, the answer is the same 403.
3. The session narrows the endpoint: 100 bytes are over its 64 (413), and a PNG, which the endpoint allows, is outside the session's types (415).
4. A second `upload.Sessions` over the same database, as another instance would build, redeems the token.
5. Once the clock passes the expiry the token is refused, swept or not; `Sweep` then deletes the row.
6. A revoked token is refused at once. A request wider than the endpoint's policy cannot be issued.

## Files

| File | What it holds |
|---|---|
| `main.go` | The Store with the plugin, `upload.Sessions`, the handler, issuing and the client's requests |
| `main_test.go` | Runs the steps |

## Next

- The guide: [Upload sessions](../../docs/plugins/upload-sessions.md); the plugin's [README](../../records/uploadsessions/README.md).
- Related examples: [concepts-uploads](../concepts-uploads) for every upload path a session can narrow, [recipe-resumable-uploads](../recipe-resumable-uploads) for tus.
