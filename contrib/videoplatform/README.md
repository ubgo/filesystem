# contrib/videoplatform

`github.com/ubgo/filesystem/contrib/videoplatform` (package `videoplatform`) hands videos to a hosted video platform, which turns them into adaptive streams (HLS) with thumbnails, and keeps each file's asset in the records layer: the platform's asset ID and state, and the playback and thumbnail URLs. It is a records plugin over a small `Platform` interface; `mux` (Mux Video) and `cfstream` (Cloudflare Stream) are the adapters, over each platform's REST API with `net/http` and no vendor SDK. The module imports nothing but the standard library, the library's own packages and `ubgo/sqlb` (`deps_test.go` enforces it).

```sh
go get github.com/ubgo/filesystem/contrib/videoplatform
```

The guide: [video, adaptive streaming](../../docs/plugins/video.md#adaptive-streaming-through-mux-or-cloudflare-stream).

## API

| Name | What it is |
|---|---|
| `Platform` | `Name`, `Ingest(ctx, Source)`, `Asset(ctx, id)`, `Delete(ctx, id)`, `Webhook(r, body)`; implement it for another platform |
| `Asset`, `Status` (`StatusPreparing`, `StatusReady`, `StatusErrored`, `StatusDeleted`, final), `Source` | the platform-neutral asset, its state and what `Ingest` fetches |
| `New(Platform, ...Option) *Plugin`, `WithSourceTTL(d)`, `WithSourceURL(fn)` | the records plugin (`PluginName` `videoplatform`, table `fs_video_assets`) |
| `WithCreateOptions(fn func(ctx, s, records.File) (map[string]json.RawMessage, error))`, `Source.Options` | create-request fields per published file (a plan's `max_resolution_tier`, a creator's watermark, a tenant's `allowedOrigins`), merged over the adapter's `Config.CreateOptions` with the file's value winning; nil (the default) sends the `Config`'s request. An error from `fn`, a reserved field or a value that is not JSON fails `Publish` before the platform is called, with no asset created |
| `Plugin.Publish`, `Plugin.Unpublish`, `Plugin.Asset`, `Plugin.Sync` | hand a file to the platform, drop it, read its asset, ask the platform for its state |
| `Plugin.Webhook(store, logger) http.Handler` | verifies the signature, records the state, sets the file `ready` or `failed`; 204 for what it acknowledges (a replayed request changes nothing), 403 for a bad signature, 400 for a signed body it cannot read, 503 to make the platform retry |
| `EventPublished`, `EventReady`, `EventFailed`, `DetailAssetID`, `DetailPlatform`, `DetailReason` | the plugin's events |
| `VerifySignature`, `Sign`, `WebhookTolerance`, `MaxWebhookBytes`, `ErrIgnoredEvent` | the webhook signing both platforms use, for adapters and fakes |
| `Secret`, `NewSecret(s)`, `DurationOf(seconds)` | a credential that prints and logs as `[redacted]` (each `Config`'s token and webhook secret); a platform's seconds as a `time.Duration`, clamped so it never wraps negative |
| `mux.New(mux.Config)`, `cfstream.New(cfstream.Config)` | the adapters; each `Config` takes credentials, the webhook secret, a base URL (a fake in tests), an `*http.Client` and a clock |
| `mux.Config.CreateOptions`, `cfstream.Config.CreateOptions` (`map[string]json.RawMessage`) | extra fields of the create request by the platform's own names: Mux `video_quality`, `mp4_support`, `max_resolution_tier`; Stream `maxDurationSeconds`, `allowedOrigins`, `watermark`. A field the adapter sets itself (Mux `inputs`, `playback_policies`, `passthrough`; Stream `url`, `meta`, `requireSignedURLs`) or a value that is not JSON is refused by `New` |

## Limits

- Tests run against in-process fakes of the documented APIs. A real account is needed for the credentials, for the platform fetching the signed source URL (the bucket must be reachable from the internet), and for webhooks reaching a public URL.

## Quarantined files

`Publish` refuses a [quarantined](../../docs/records/processing-status.md#quarantine-until-scanned) file (`records.ErrQuarantined`) before calling the platform, whatever `WithSourceURL` does; publish it after its scan passes.
