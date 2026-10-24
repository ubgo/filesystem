# Webhooks

The `webhooks` plugin delivers chosen events (file changes, and plugin events such as `quota.usage_changed` or `egress.daily_total`) to URLs registered per scope, signed in the [Standard Webhooks](https://www.standardwebhooks.com) format, retried with backoff through a job, and never sent to an address outside the public internet. Package `github.com/ubgo/filesystem/records/webhooks`; reference in its [README](../../records/webhooks/README.md).

## Before you start

You have a records Store ([Files](../records/files.md)) and a way to run jobs ([jobs](../../jobs/README.md)). Read [Events and hooks](../records/events-and-hooks.md) for the event kinds.

The example is [`examples/records-webhooks`](../../examples/records-webhooks): `go run ./records-webhooks` in `examples/` prints every step below; `go test ./records-webhooks` runs them. Its receiver listens on 127.0.0.1 in the same process, which the default client refuses (see [Security](#security)), so the example passes its own client with `WithClient`.

When to use it: other systems (a customer's integration, your billing service, a search indexer in another deployment) must learn about file changes. Inside the same process, an [`EventHooks` plugin](../records/events-and-hooks.md) is simpler and transactional.

## Step 1 — Install the plugin

<!-- ds:block id=newwebhookstore-x6t3n9qc -->
**newStore** · [`examples/records-webhooks/main.go:76-102`](../../examples/records-webhooks/main.go#L76-L102)

```go
func newStore(ctx context.Context, db *sql.DB, now func() time.Time, client *http.Client) (*records.Store, *webhooks.Plugin, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	wh, err := webhooks.New(
		webhooks.WithClient(client),
		webhooks.WithBackoff(func(int) time.Duration { return retryAfter }),
		webhooks.WithMaxAttempts(5),
		webhooks.WithLogger(slog.New(slog.DiscardHandler)), // the demo prints the state instead
	)
	if err != nil {
		return nil, nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithClock(now), records.WithPlugins(wh))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, wh, nil
}
```
<!-- /ds:block hash=390509 -->

`Migrate` creates `fs_webhook_endpoints` and `fs_webhook_deliveries`. Endpoint and delivery IDs come from the Store's generator (`Store.NewID`), so an application's own scheme set with `records.WithIDs` covers them, as in this sketch with UUIDv7 IDs (`github.com/google/uuid`):

```go
s, err := records.New(db, dialect, disks,
	records.WithIDs(func(prefix string) string { return prefix + uuid.Must(uuid.NewV7()).String() }),
	records.WithPlugins(wh))
```

Options: `WithSealer(s)` (seal secrets at rest, below), `WithMaxAttempts(n)` (default 10), `WithBackoff(fn)` (default `DefaultBackoff`: 30 seconds, doubling, at most 6 hours), `WithLogger(l)`, and `WithClient(c)`, the escape hatch for a proxy or mutual TLS, which hands the address guard to your client.

## Step 2 — Register an endpoint

<!-- ds:block id=register-m4w8k2tv -->
**register** · [`examples/records-webhooks/main.go:108-110`](../../examples/records-webhooks/main.go#L108-L110)

```go
func register(ctx context.Context, s *records.Store, wh *webhooks.Plugin, url string) (webhooks.Endpoint, error) {
	return wh.AddEndpoint(ctx, s, tenant, url, records.EventFileCreated, records.EventFilePurged)
}
```
<!-- /ds:block hash=44dbda -->

Show `ep.Secret.Reveal()` (`whsec_…`) to the receiver's owner once, for checking signatures.

No kinds means every kind. The URL must be http or https, with a host and no credentials. The secret prints, logs (`slog`) and marshals to JSON as `[redacted]`; only `Reveal` returns it. `Endpoints` lists a scope's endpoints, `RemoveEndpoint` deletes one.

## Step 3 — Deliver

Each matching event writes a pending delivery inside the change's own transaction (an outbox): a change that rolls back sends nothing, one that commits is never lost. Schedule `jobs.WebhookDeliver(wh, store)` every few seconds to a minute; each run claims each due delivery just before posting it, with a five-minute lease from the Store's clock at that moment and the attempt cut off well inside it, so however long a batch runs, no other run can send the same delivery meanwhile, and the outcome is written only while the claim is still the run's:

| Header | Holds |
|---|---|
| `Webhook-Id` | the event's ID (`records.Event.ID`), the same on every attempt |
| `Webhook-Timestamp` | Unix seconds of the attempt itself, never of the batch's start |
| `Webhook-Signature` | `v1,` and the base64 HMAC-SHA256 of `id.timestamp.body` |

The body is a `webhooks.Payload`: the event's fields and, for file events, the file's ID, name, type, size, SHA-256, visibility and status (never its disk or key). A 2xx answer delivers it; any other answer, a redirect included, or a connection error schedules the next attempt after the backoff, and after the last attempt the delivery is marked `failed`. Deliveries are at least once: the receiver drops IDs it has handled.

Check: the example records a file and prints `pending after the upload: 1 (file.created)`. Its receiver answers 503 to the first attempt, so `deliver (first): 0 delivered` is followed by `still pending, attempts 1, last error "webhooks: endpoint answered 503 Service Unavailable"`. A run before the backoff sends nothing (`deliver (too early): 0 delivered`); one after it prints `deliver (after backoff): 1 delivered`, and `Deliveries(…, webhooks.StateDelivered)` shows it delivered after two attempts.

## Step 4 — Receive

Any Standard Webhooks library checks these deliveries. In Go:

<!-- ds:block id=receiver-q7n2c5hw -->
**receiver** · [`examples/records-webhooks/main.go:116-122`](../../examples/records-webhooks/main.go#L116-L122)

```go
type receiver struct {
	mu     sync.Mutex
	secret webhooks.Secret
	seen   map[string]bool
	calls  int
	got    []string
}
```
<!-- /ds:block hash=bc3b4f -->

It refuses a changed body (the test `TestTamperedBodyRefused`), another secret, or a timestamp more than five minutes away (a replay). Cap the body before reading it, check the signature before parsing it, and answer 2xx only once the event is handled.

## Other models: global endpoints, secret rotation, signers and payloads

Each of these keeps the default unchanged when not used.

- **One consumer for every tenant.** `AddGlobalEndpoint(ctx, s, url, kinds...)`, or `Register` with `EndpointSpec{Global: true}`, takes the events of every scope: a search indexer or warehouse loader. List them with `GlobalEndpoints` and remove them with `RemoveGlobalEndpoint`; `Endpoints(scope)` never shows them. Never hand one to a tenant.
- **A secret you already have.** `Register(ctx, s, webhooks.EndpointSpec{Scope, URL, Kinds, Secret})` keeps a caller's `whsec_` secret of 24 to 64 bytes (`CheckSecret`), for receivers migrated from another sender. A signer whose secrets have another shape (GitHub-style receivers that already hold a free-form secret, ed25519 key material) also implements `webhooks.SecretFormat` (`NewSecret() (Secret, error)` and `CheckSecret(Secret) error`); `Register` and `RotateSecret` then make and check secrets with it. The floor stays: whatever the format, a secret is 24 (`MinSecretBytes`) to 1024 bytes without whitespace.
- **Rotation without downtime.** `RotateSecret(ctx, s, webhooks.EndpointRef{Scope, ID}, next)` adds a secret (a random one when `next` is empty) and keeps the old ones; every delivery is then signed with each, space-separated in `Webhook-Signature` as Standard Webhooks allows, so receivers on the old and the new secret both accept it. Once every receiver has the new one, `RetireSecrets` drops the rest. At most `MaxSecrets` (5) stay active.
- **Another signature scheme.** `WithSigner(sg)` replaces `StandardSigner` with any `Signer`: ed25519 (Standard Webhooks v1a), GitHub-style `X-Hub-Signature-256`, or a key that never leaves a KMS. A signer that fails fails the attempt, which is retried; nothing is sent unsigned.
- **Another body.** `WithPayload(fn)` builds each event's JSON body, for example a CloudEvents envelope around `DefaultPayload(e)`. An error from `fn` fails the change, so no event is dropped or sent in a shape receivers were not promised.
- **A body per endpoint.** `WithEndpointPayload(fn)` builds each delivery's body with `fn(ctx, event, endpoint)` returning a `webhooks.Body{Data, ContentType, Header}`, so a Slack receiver and a CloudEvents one each get their own shape. `ContentType` defaults to `application/json`; `application/cloudevents+json` is CloudEvents' structured mode, and `Header` carries the `ce-*` headers of its binary mode. The body and headers are stored with the delivery, so a retry sends the same request. An error, a body that is not valid UTF-8, or a `Webhook-*`, `Host`, `Content-Length`, `Transfer-Encoding` or `Connection` header fails the change; the signer runs after the headers are set, so its headers always win.
- **High volume.** `WithBatch(n)` and `WithConcurrency(n)` set how many deliveries a `Deliver` run takes (default 100) and sends at once (default 1). Each delivery is still claimed on its own before it is sent.

```go
p, err := webhooks.New(webhooks.WithPayload(func(_ context.Context, e records.Event) (any, error) {
	return map[string]any{
		"specversion": "1.0", "id": e.ID, "type": string(e.Kind), "source": "/scopes/" + e.Scope,
		"time": e.At, "data": webhooks.DefaultPayload(e),
	}, nil
}), webhooks.WithConcurrency(8), webhooks.WithBatch(500))
```

A body per endpoint, CloudEvents binary mode for one receiver and a Slack message for another:

```go
p, err := webhooks.New(webhooks.WithEndpointPayload(func(_ context.Context, e records.Event, ep webhooks.Endpoint) (webhooks.Body, error) {
	if strings.HasPrefix(ep.URL, "https://hooks.slack.com/") {
		msg, err := json.Marshal(map[string]string{"text": string(e.Kind) + " " + e.FileID})
		return webhooks.Body{Data: msg}, err
	}
	data, err := json.Marshal(webhooks.DefaultPayload(e))
	return webhooks.Body{Data: data, Header: http.Header{
		"Ce-Specversion": {"1.0"}, "Ce-Id": {e.ID}, "Ce-Type": {string(e.Kind)}, "Ce-Source": {"/scopes/" + e.Scope},
	}}, err
}))
```

Try it: [`examples/records-webhooks`](../../examples/records-webhooks) runs this section in `models.go` ([`githubSigner`](ds:block?id=githubsigner-w8r3k6pz), [`cloudEvent`](ds:block?id=cloudevent-c4n9t2xa)): `go run ./records-webhooks` from `examples/`.

## Security

The default client is `upload.FetchOptions{MaxRedirects: -1}.Client()` with a 10-second timeout: it connects only to addresses `upload.PublicAddr` accepts, checked after DNS resolution on every connection, so a name that resolves or rebinds to a private, loopback, link-local (cloud metadata) or other internal address is refused, and `AddEndpoint` refuses such a literal address up front. It uses no proxy and follows no redirect. Secrets are stored in the table to sign with, as they are by default. `WithSealer(s)` takes a `webhooks.Sealer` (`Seal(ctx, secret)` and `Open(ctx, sealed)`), backed by a KMS or an AEAD whose key lives outside the database, and stores only the sealed text.

## Step 5 — Prune

Schedule `jobs.WebhookPrune(wh, store, retention)`: it deletes delivered and failed deliveries older than the retention; pending ones are kept. `Deliveries(ctx, store, scope, state)` lists them for an admin screen.

## The tables

`fs_webhook_endpoints` (`id`, `scope`, `url`, `secret` (every active secret, space-separated, sealed as a whole with `WithSealer`), `kinds`, `created_at`, `is_global` from `webhooks/0002_webhooks_global`) and `fs_webhook_deliveries` (`id`, `endpoint_id`, `scope`, `event_id`, `kind`, `payload`, `state`, `attempts`, `next_at`, `last_error`, `created_at`, `updated_at`, `headers` from `webhooks/0003_webhooks_headers`: `""` for a default JSON body, else the JSON of the headers `WithEndpointPayload` chose; indexes `(state, next_at)` and `(endpoint_id, created_at)`), migration `webhooks/0001_webhooks`.

## What can go wrong

- Deliveries stay pending with `last_error` naming "public internet" — the endpoint resolves to an internal address. Use a public URL, or `WithClient` with a guard of your own for an internal receiver.
- `failed` with `endpoint removed` — the endpoint was deleted while deliveries were pending.
- The receiver refuses every delivery — its clock is more than five minutes off, or it checks against another endpoint's secret.
- `AddEndpoint` refuses `http://localhost/…` or `http://10.…` in development — the default client's guard. Point it at a public tunnel, or pass `WithClient` in development only, as the example does.
- A receiver processes an event twice — it answered non-2xx after handling it (a timeout, a 500 after the write), so the delivery was retried. Keep the IDs you handled.

## Try it

```sh
cd examples
go run ./records-webhooks
go test ./records-webhooks
```
