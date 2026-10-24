# contrib/vision

`github.com/ubgo/filesystem/contrib/vision` (package `vision`) tags image files with what they show. A `Labeler` names what an image shows; the records plugin writes the labels of every image recorded in an opted-in scope as tags through [records/tags](../../records/tags/README.md), after the upload commits, and `Plugin.Label` does the same on demand. The subpackage `google` is a `Labeler` for Google Cloud Vision's label detection over plain REST, with the API key sent in a header and held as a `Secret` that never prints. It is its own module because it calls a vendor's service; the core never does, and this module uses no vendor SDK (`deps_test.go` enforces it).

```sh
go get github.com/ubgo/filesystem/contrib/vision
```

The guide, with Google's prices: [Auto-tagging](../../docs/plugins/vision.md). The example: [`examples/documents/labels`](../../examples/documents/labels/main.go).

## API

| Name | What it is |
|---|---|
| `Labeler`, `Label{Name, Score}` | the seam: one method, `Labels(ctx, image) ([]Label, error)` |
| `NewPlugin(Labeler, LabelSink, scopes, ...Option) *Plugin` | the records plugin (`PluginName` `vision`); `scopes` opts scopes in, nil labels none |
| `LabelSink` (`Add`, `Tags`) | where labels go: `*tags.Plugin` is the default and implements it; another sink sends them to a search index, a metadata key or a moderation table. Its error is `Label`'s |
| `Plugin.Label(ctx, store, scope, id) ([]string, error)` | labels one file now and returns its tags |
| `WithMinScore`, `WithMaxLabels`, `WithPrefix`, `WithContentTypes`, `WithMaxInput`, `WithErrorHandler` | the options |
| `DefaultContentTypes()`, `DefaultMinScore`, `DefaultMaxLabels`, `DefaultMaxInput` | the defaults |
| `Secret`, `NewSecret` | a credential that prints and logs as `[redacted]` |
| `google.New(google.Config) (*google.Client, error)` | the Cloud Vision labeler; `Config{APIKey, Endpoint, HTTP, MaxResults, MaxImage}` |
| `google.Config.APIKey` with `HTTP` | `APIKey` is optional when `HTTP` authenticates the requests itself (an OAuth2 transport with a service account or workload identity), for organisations that ban API keys; no key header is sent then. Neither is `invalid_argument` |

## Limits

- Tested against an in-process fake of `images:annotate`; a real run needs a Google Cloud API key and is billed per image.
- Replaced content is not relabelled automatically.

## Quarantined files

A [quarantined](../../docs/records/processing-status.md#quarantine-until-scanned) image is never sent to the labeler: not on upload, and `Label` refuses it (`records.ErrQuarantined`); it is labelled after its scan passes (`file.released`).
