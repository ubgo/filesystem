# github.com/ubgo/filesystem/records/webhooks

A records plugin that delivers chosen events to URLs registered per scope, signed in the Standard Webhooks format, retried with backoff through `jobs.WebhookDeliver`, and guarded against internal addresses with `upload.PublicAddr`. The guide is [docs/plugins/webhooks.md](../../docs/plugins/webhooks.md).

## Install

```sh
go get github.com/ubgo/filesystem/records
```

Part of the records module; it uses only the standard library, the core and `upload` from the core module, whose guarded client (`upload.FetchOptions.Client`) it sends with.

## Go API

| Symbol | What it does |
|---|---|
| `New(opts...)` | The plugin; `CodeInvalidArgument` for a nil client, backoff or logger, or fewer than one attempt |
| `WithClient(c)` | Replaces the guarded default client (the escape hatch); the client must guard addresses itself |
| `WithBackoff(fn)`, `DefaultBackoff` | Wait after the n-th failure; default 30 s doubling to 6 h |
| `WithMaxAttempts(n)`, `DefaultMaxAttempts` | Attempts before `StateFailed`; default 10 |
| `WithSealer(s)`, `Sealer` | Seals endpoint secrets at rest (`Seal`/`Open`); plaintext without one |
| `WithLogger(l)` | Where failed attempts are logged; default `slog.Default()` |
| `WithSigner(sg)`, `Signer`, `SignRequest`, `StandardSigner` | Signs each delivery; default `StandardSigner` (Standard Webhooks v1, one signature per active secret). Plug in ed25519, GitHub-style or KMS signing; a failing signer fails the attempt, which is retried, never sent unsigned |
| `WithPayload(fn)`, `PayloadFunc`, `DefaultPayload(e)` | Builds each event's JSON body with `fn(ctx, event) (any, error)` (a CloudEvents envelope, a trimmed body); an error fails the change. Default `DefaultPayload` |
| `WithEndpointPayload(fn)`, `EndpointPayloadFunc`, `Body{Data, ContentType, Header}` | Builds each delivery's body per endpoint with `fn(ctx, event, endpoint)`: a Slack message for one receiver, CloudEvents structured (`application/cloudevents+json`) or binary (`ce-*` headers) for another, a non-JSON body. The body and headers are stored with the delivery, so a retry sends the same bytes. An error, a body that is not valid UTF-8, or a `Webhook-*`, `Host` or framing header fails the change; the signer runs after and always wins. Default: `WithPayload`'s JSON for every endpoint, as `application/json` (`DefaultContentType`, also what an empty `Body.ContentType` sends) |
| `WithBatch(n)`, `WithConcurrency(n)` | Deliveries one `Deliver` run takes (default `DefaultBatch`, 100) and sends at once (default `DefaultConcurrency`, 1); each delivery is still claimed on its own |
| `(*Plugin).AddEndpoint(ctx, s, scope, url, kinds...)` | Registers an endpoint (all kinds when none) with a new `Secret` |
| `(*Plugin).Register(ctx, s, EndpointSpec{Scope, URL, Kinds, Secret, Global})` | Registers an endpoint with a caller's secret (`whsec_` and 24 to 64 bytes; `CheckSecret`, or the signer's `SecretFormat`), or a global one |
| `SecretFormat` (`NewSecret()`, `CheckSecret(secret)`) | An optional `Signer` upgrade for secrets that are not `whsec_` base64 (a GitHub-style free-form secret, ed25519 key material): `Register` and `RotateSecret` then generate and check with it. The floor holds whatever the format: `MinSecretBytes` (24) to `MaxSecretTextBytes` (1024) bytes, no whitespace |
| `(*Plugin).AddGlobalEndpoint(ctx, s, url, kinds...)`, `GlobalEndpoints`, `RemoveGlobalEndpoint` | An endpoint for the events of every scope (one internal consumer); never hand one to a tenant |
| `(*Plugin).RotateSecret(ctx, s, EndpointRef{Scope, ID, Global}, next)`, `RetireSecrets(ctx, s, ref)` | Zero-downtime rotation: up to `MaxSecrets` (5) active secrets, each signing every delivery (`Endpoint.Secrets`, newest first), then retire the old ones; `NewSecret()` makes one |
| `(*Plugin).Endpoints(ctx, s, scope)`, `RemoveEndpoint(ctx, s, scope, id)` | List and delete a scope's endpoints (global ones excluded) |
| `(*Plugin).OnEvent` | `records.EventHooks`: writes a pending delivery per matching endpoint inside the change's transaction |
| `(*Plugin).Deliver(ctx, s, now)` | Claims and posts up to `DefaultBatch` due deliveries; returns how many were delivered |
| `(*Plugin).Deliveries(ctx, s, scope, state)` | Lists deliveries in `StatePending`, `StateDelivered` or `StateFailed` |
| `(*Plugin).Prune(ctx, s, cutoff)` | Deletes delivered and failed deliveries last updated before `cutoff` |
| `Secret` | Prints, logs and marshals as `[redacted]`; `Reveal()` returns it |
| `Sign(h, secret, id, at, body)`, `SignAll(h, secrets, id, at, body)`, `VerifySignature(h, secret, body, now, tolerance)` | The signature on both sides; `HeaderID`, `HeaderTimestamp`, `HeaderSignature`, `DefaultTolerance` |
| `Payload`, `FilePayload` | The JSON body; no disk or storage key |

## Tables

`fs_webhook_endpoints` and `fs_webhook_deliveries`, migration `webhooks/0001_webhooks`, `webhooks/0002_webhooks_global` adding `fs_webhook_endpoints.is_global`, and `webhooks/0003_webhooks_headers` adding `fs_webhook_deliveries.headers` (`""` for a default JSON body, else the JSON of the request headers `WithEndpointPayload` chose); the `secret` column holds every active secret, space-separated, sealed as a whole with `WithSealer`; typed columns `EndpointTable`, `Endpoint*`, `DeliveryTable`, `Delivery*`.
