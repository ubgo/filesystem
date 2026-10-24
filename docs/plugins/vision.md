# Auto-tagging images

You will have every image uploaded to the scopes you choose tagged with what it shows (`dog`, `beach`, `invoice`), so people can search and group pictures nobody tagged by hand. The package is `contrib/vision`: a small `Labeler` interface, a records plugin that writes labels through the [tags plugin](tags.md), and `contrib/vision/google`, a labeler for Google Cloud Vision over its plain REST API.

## Before you start

- A Google Cloud project with the Cloud Vision API enabled and an API key restricted to it (Google Cloud console, APIs and services, Credentials). Keep the key in your secret store; the example reads it from `VISION_API_KEY`.
- The tags plugin installed in the Store: labels are tags.
- `go get github.com/ubgo/filesystem/contrib/vision`. It is its own module because it calls a vendor's service; the core never does. It uses no Google SDK, only `net/http`.
- The example is [`examples/documents/labels`](../../examples/documents/labels/main.go), in the separate module `examples/documents`.

## What it costs

Each labelled image is one billable unit of Google's label detection. As of October 2026, Google's price list says: the first 1000 units a month are free, then $1.50 per 1000 up to 5 million a month, then $1.00 per 1000 ([Cloud Vision pricing](https://cloud.google.com/vision/pricing)). A scope with 100,000 photo uploads a month costs about $148.50. That is why labelling is off for every scope until you opt one in, why an image over the size bound is refused before it is sent, and why the plugin never relabels on its own.

## What it guards against

The API key travels in the `X-Goog-Api-Key` header, never in the URL, so it is in no error message or proxy log; the client refuses every redirect, because Go's HTTP client copies custom headers onto a redirect to another host (it works on a copy of a `Config.HTTP` you pass, which is left unchanged); and `google.Config` holds it as a `vision.Secret`, which prints and logs as `[redacted]`. An image over `google.Config.MaxImage` (7 MiB, Google's request limit once base64-encoded) or the plugin's `WithMaxInput` is `CodeTooLarge` and is not sent. The response read is bounded, and Google's error messages are cut short. Labels become tags only when they are valid tags (`tags.Check`), so a long or odd label is skipped rather than failing the file.

## Step 1 — The plugin

`vision.NewPlugin(labeler, tagsPlugin, scopes, opts...)` labels every image (JPEG, PNG, GIF, WebP by default) recorded in a scope `scopes` accepts, once the upload has committed, so a rolled-back upload is never sent; the upload itself never fails because of labelling, and failures go to `WithErrorHandler` (by default logged with `slog.Default`). It keeps labels scored at least `WithMinScore` (0.7), the best `WithMaxLabels` (10), and adds them with `tags.Add`, after any `WithPrefix`, so the file keeps the tags people gave it:

<!-- ds:block id=newplugins-wbgpbquv -->
**newPlugins** · [`examples/documents/labels/main.go:47-62`](../../examples/documents/labels/main.go#L47-L62)

```go
func newPlugins(key, endpoint string) (*tags.Plugin, *vision.Plugin, error) {
	labeler, err := google.New(google.Config{APIKey: vision.NewSecret(key), Endpoint: endpoint})
	if err != nil {
		return nil, nil, err
	}
	t := tags.New()
	v := vision.NewPlugin(labeler, t,
		func(scope string) bool { return scope == labelled },
		vision.WithPrefix("auto:"),
		vision.WithMinScore(0.8),
		vision.WithErrorHandler(func(_ context.Context, f records.File, err error) {
			log.Printf("labelling %s failed: %v", f.Filename, err)
		}),
	)
	return t, v, nil
}
```
<!-- /ds:block hash=967530 -->

Check: `VISION_API_KEY=... go run ./labels dog.jpg` from `examples/documents` prints the file and its tags, as the example's test does against a stand-in for Google: `dog.png  [auto:dog auto:grass]`.

## Step 2 — Label on demand

`plugin.Label(ctx, store, scope, fileID)` labels one file now and returns its tags. Use it to backfill images recorded before you opted a scope in, or from a worker when the call should not run in the request: the plugin then need not be installed in the Store at all (the tags plugin must be). It refuses a scope not opted in or a type not labelled (`CodeInvalidArgument`) and a file over the bound (`CodeTooLarge`) before calling the labeler.

## Step 3 — Another labeler

`vision.Labeler` is one method, `Labels(ctx, image io.Reader) ([]vision.Label, error)`, with labels as `Label{Name, Score}` (0 to 1). Wrap a self-hosted model or another vendor in it; the tests plug in a fake.

## Step 4 — Labels somewhere other than tags

The second argument of `NewPlugin` is a `vision.LabelSink` (`Add` and `Tags`, the methods `*tags.Plugin` has). Send labels to a search index, a metadata key or a moderation table instead:

```go
type indexSink struct{ idx *search.Index }

func (s indexSink) Add(ctx context.Context, _ *records.Store, scope, fileID string, names ...string) ([]string, error) {
	return s.idx.AddLabels(ctx, scope, fileID, names) // your index
}
func (s indexSink) Tags(ctx context.Context, _ *records.Store, scope, fileID string) ([]string, error) {
	return s.idx.Labels(ctx, scope, fileID)
}

plugin := vision.NewPlugin(labeler, indexSink{idx}, optedIn)
```

Names arrive prefixed, best first, each a valid tag name. A sink error is `Label`'s error, and after an upload it goes to `WithErrorHandler`.

For Cloud Vision without an API key (organisations that ban them), leave `APIKey` empty and pass an `HTTP` client that authenticates itself, such as an OAuth2 transport from `golang.org/x/oauth2/google` with a service account; no key header is then sent. With neither, `google.New` is `invalid_argument`.

## Options

| Option | What it does |
|---|---|
| `scopes func(string) bool` | the scopes labelled; nil labels none |
| `vision.WithMinScore(f)` | keep labels scored at least `f`; default 0.7 |
| `vision.WithMaxLabels(n)` | keep at most `n` labels, best first; default 10; a file has at most `tags.MaxPerFile` tags in all |
| `vision.WithPrefix(p)` | put `p` before every label (`auto:`), so labels can be filtered or removed as a set |
| `vision.WithContentTypes(...)` | the types labelled; default `vision.DefaultContentTypes()` |
| `vision.WithMaxInput(n)` | bytes of an image sent; default 7 MiB |
| `vision.WithErrorHandler(fn)` | receives labelling failures after an upload commits |
| `google.Config{APIKey, Endpoint, HTTP, MaxResults, MaxImage}` | the key (required unless `HTTP` authenticates the requests), a regional endpoint or a fake, the HTTP client (30-second timeout by default), labels asked for (10), image bound (7 MiB) |

## Pitfalls

- **Labelling runs after the commit, in the request.** `AfterCommit` work runs before the upload call returns; a Google call adds its latency to the request. Call `Label` from a job for large volumes.
- **Replaced content is not relabelled.** The tags stay with the file; call `Label` again after a replacement if the picture changed.
- **Errors are Google's deployment errors too.** An invalid key is a 400 from Google; the labeler reports every HTTP failure as `CodeUnavailable` (a deployment problem), and an image Google cannot decode as `CodeRejected`.
- **What needs a real account.** Everything in the package is tested against an in-process fake of `images:annotate`; only the example's real run needs a key, and it bills.

## Quarantined files

A [quarantined](../records/processing-status.md#quarantine-until-scanned) image is never sent to the labeler: not on upload, and `Label` refuses it (`records.ErrQuarantined`); it is labelled after its scan passes (`file.released`).

## Next

- [Tags](tags.md): querying files by tag, counts and renames.
- [OCR](ocr.md): what an image says rather than what it shows.
