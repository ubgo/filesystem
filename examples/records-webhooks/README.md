# Signed outbound webhooks

The `webhooks` plugin of the records layer: an endpoint registered for chosen event kinds, a delivery written in the change's own transaction (an outbox), signed in the Standard Webhooks format, retried after a backoff, and recorded as delivered. The receiver runs in the same process on 127.0.0.1, which the plugin's default client refuses, so the demo passes its own client with `WithClient`. The guide [Webhooks](../../docs/plugins/webhooks.md) explains each step.

## Run it

```sh
cd examples
go run ./records-webhooks
go test ./records-webhooks
```

It needs no network beyond the loopback receiver, and no environment variables.

## What it does

```text
endpoint whe_… secret prints as [redacted], starts whsec_ true
pending after the upload: 1 (file.created)
deliver (first): 0 delivered
  still pending, attempts 1, last error "webhooks: endpoint answered 503 Service Unavailable"
deliver (too early): 0 delivered
deliver (after backoff): 1 delivered
delivered: 1 after 2 attempt(s); receiver got file.created report.txt
global indexer got 2: file.created /scopes/acme, file.created /scopes/globex
acme's portal got 1: file.created /scopes/acme; Endpoints(acme) lists 1
body is a CloudEvent: true; X-Hub-Signature-256 valid: true
rotating: 2 signature(s); old secret verifies true, new secret verifies true
retired: 1 signature(s); old secret verifies false, new secret verifies true
```

1. `AddEndpoint` registers the receiver for `file.created` and `file.purged`. Its secret prints as `[redacted]`; only `Reveal` returns it.
2. Recording a file writes one pending delivery inside the same transaction.
3. The first `Deliver` posts it; the receiver checks the signature and answers 503, so the delivery stays pending with the error and one attempt.
4. A run before the backoff has passed sends nothing; one after it delivers.
5. The receiver keeps the event IDs it handled, because deliveries are at least once.
6. `models.go` opens a second Store with `webhooks.WithPayload(cloudEvent)` and `webhooks.WithSigner(githubSigner)`. `AddGlobalEndpoint` registers a search indexer for every scope, beside an endpoint of `acme`'s own. One file recorded in `acme` and one in `globex` reach the indexer both, and `acme`'s endpoint only its own; `Endpoints(acme)` never lists the global one.
7. Every body is a CloudEvents envelope around `DefaultPayload`, and every delivery carries a GitHub-style `X-Hub-Signature-256` beside the Standard Webhooks headers, which the receiver checks.
8. `RotateSecret` adds a new secret and keeps the old one: the next delivery carries two signatures, so a receiver on either secret accepts it. After `RetireSecrets` only the new secret signs.

`go test` also pins that the default client refuses a loopback endpoint, that a changed body fails the signature check, and that a `Signer` that fails sends nothing and leaves the delivery pending with its error.

## Files

| File | What it holds |
|---|---|
| `main.go` | The Store with the plugin, the clock, the receiver and the steps |
| `models.go` | A global endpoint, secret rotation, a signer of the app's own and a CloudEvents body; code: [`newModelStore`](ds:block?id=newmodelstore-h6q2v8tm), [`rotateDemo`](ds:block?id=rotatedemo-b2m7x4kc) |
| `main_test.go` | Runs the steps, the address guard, a tampered body and a failing signer |

## Next

- The guide: [Webhooks](../../docs/plugins/webhooks.md); the plugin's [README](../../records/webhooks/README.md).
- Related examples: [records-events](../records-events) for the event kinds, [records-egress](../records-egress) for the billing events webhooks carry, [jobs-scheduler](../jobs-scheduler) to run `Deliver` on a schedule.
