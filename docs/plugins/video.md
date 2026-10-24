# Video metadata, posters, transcodes and streaming

You will have every uploaded video checked, cleaned and described: refused when it is not a readable video or is too long, stripped of the metadata a phone writes into it (the place it was recorded among it), and stored with its duration, display size and codecs on its file row and a poster frame beside it, so a page can show a player of the right shape and a thumbnail without touching the video again. The package is `contrib/video`, which runs FFmpeg's `ffprobe` and `ffmpeg` programs.

## Before you start

- FFmpeg installed where the server runs (`brew install ffmpeg`, `apt install ffmpeg`, the `ffmpeg` package of your container's distribution). The programs are found on `PATH`, or set `video.Tool.FFprobe` and `video.Tool.FFmpeg` to their paths.
- `go get github.com/ubgo/filesystem/contrib/video`. It is its own module because it runs external programs; the core never does.
- The example is [`examples/media/video`](../../examples/media/video/main.go), in the separate module `examples/media`: `go run ./video clip.mp4` from `examples/media` uploads a file, prints what was recorded, then makes a web MP4 copy and a storyboard; `go test ./video` generates a clip with ffmpeg and checks the report. Streaming through a platform is [`examples/media/streaming`](../../examples/media/streaming), which needs neither FFmpeg nor an account.
- [Uploads](../concepts/uploads.md) explains `Process` and `Inspect`; [files as records](../records/README.md) explains plugins.

## What it guards against

A video is untrusted input to a large C program, so every call is bounded: at most `Tool.MaxInput` bytes (2 GiB by default) are copied to a work file, each program run is killed after `Tool.Timeout` (one minute), and its output is capped (`Tool.MaxPoster`, 10 MiB for a poster). Programs run through `os/exec` without a shell, with the demuxers limited to `Tool.Formats` (the containers phones, cameras and browsers record) and the protocols to `file`, so a playlist or concat script disguised as a video cannot make FFmpeg open other files or URLs. A file FFmpeg cannot read is `CodeRejected` with a message that names no server path (`video: ffprobe could not read the file`); the program's own output is in `video.ProgramError.Detail` for your logs.

## Step 1 — The upload policy

`Tool.Sanitize()` is a `Policy.Process` that remuxes MP4, QuickTime, WebM and Matroska files without re-encoding: container and stream tags (location, device, title), chapters and every stream but video and audio are dropped, so it is fast and lossless. `Tool.Inspect(limits)` is a `Policy.Inspect` that probes the stored file and refuses what ffprobe cannot read as a video or what is over the limits; other content types pass both untouched:

<!-- ds:block id=policy-6uud9sgf -->
**policy** · [`examples/media/video/main.go:47-53`](../../examples/media/video/main.go#L47-L53)

```go
var policy = upload.Policy{
	MaxSize: 200 << 20,
	Allowed: []string{"video/*"},
	Key:     upload.RandomKey("videos"),
	Process: tool.Sanitize(),
	Inspect: tool.Inspect(video.Limits{MaxDuration: maxDuration, MaxWidth: 3840, MaxHeight: 2160}),
}
```
<!-- /ds:block hash=1d6016 -->

## Step 2 — The records plugin

`video.NewPlugin(tool, opts...)` describes every video file when it is recorded and when its content is replaced. It writes `duration_ms`, `width`, `height` (the same keys the [dimensions](dimensions.md) plugin uses for images, so one layout reads both), `video_codec` and `audio_codec` into the file's metadata. `WithPoster(at)` also stores the frame at `at` (the first frame of a shorter video) as a JPEG next to the video and records its key as `poster_key`. The poster follows the file: written before the row and deleted again if the transaction rolls back, deleted after a purge commits, and replaced with the content:

<!-- ds:block id=newstore-q787sz3p -->
**newStore** · [`examples/media/video/main.go:87-110`](../../examples/media/video/main.go#L87-L110)

```go
func newStore(ctx context.Context) (*records.Store, *filesystem.Disk, error) {
	disk, err := filesystem.NewDisk(diskMedia, memory.New())
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1) // one connection keeps the in-memory database alive
	plugin := video.NewPlugin(tool, video.WithPoster(posterAt))
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(plugin))
	if err != nil {
		return nil, nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return store, disk, nil
}
```
<!-- /ds:block hash=25a2b4 -->

Read it back with `video.Of(file)` (a `video.Meta` with the duration, size and codecs) and `video.PosterKey(file)`.

Check: record a portrait phone video carrying a location:

```sh
cd examples/media
ffprobe -v error -show_entries format_tags -of flat phone.mp4
go run ./video phone.mp4
```

```text
format.tags.major_brand="isom"
format.tags.minor_version="512"
format.tags.compatible_brands="isomiso2avc1mp41"
format.tags.title="holiday"
format.tags.encoder="Lavf62.12.101"
format.tags.location_eng="+48.8577+002.2950/"
format.tags.location="+48.8577+002.2950/"
stored    videos/svkjb4mapmtwnt6qdjgnzlpmor.mp4 (video/mp4, 100413 bytes)
duration  4s
size      720x1280
codecs    h264 / aac
poster    videos/svkjb4mapmtwnt6qdjgnzlpmor.mp4.poster-g2b6tx7pbe.jpg (image/jpeg, 24477 bytes)
```

The file is stored 1280×720 with a 90° display rotation; the recorded size is the upright 720×1280. The stored copy has no `title` or `location` left. A file cut short, and a text file named `.mp4`, are refused:

```text
2026/10/06 11:40:27 filesystem: rejected: video: ffmpeg could not read the file
2026/10/06 11:40:27 filesystem: rejected: content type "text/plain; charset=utf-8" is not allowed
```

### Posters on another disk

By default a poster sits beside its video, on the video's disk. `video.WithLocation` puts it elsewhere: a public CDN disk while the videos stay private, a cheaper bucket, or a path your CDN rule matches. The function gets the file and the derived object's name (`video.DerivedPoster`) and returns a disk name of the Store's Manager (`""` for the video's own) and a key prefix; the plugin appends a random part and `.jpg`, so a replacement's poster never overwrites the current one before the replacement commits:

```go
plugin := video.NewPlugin(tool, video.WithPoster(time.Second),
	video.WithLocation(func(f records.File, name string) (string, string, error) {
		return "cdn", "posters/" + f.Scope + "/" + f.ID + "-", nil
	}))
```

The poster's disk is recorded as `poster_disk` when it is not the video's, and `video.PosterLocation(f)` returns the disk and the key; the purge and a replacement delete the poster there. An error from the function fails the upload, and nothing is written.

Derived objects are no orphans: the plugin implements `records.ObjectOwner`, so `Store.SweepOrphans` keeps every poster a file row names (live or trashed) and deletes only one whose file was purged, and `records.FileDiskMover`, so `Store.MoveFile` takes the poster on the file's own disk along to the new disk under the same key. One `WithLocation` put on a disk of its own stays where it is. A poster missing from the disk fails the move, as a missing variant does, rather than leaving the file pointing at nothing. And `records.DiskRenamer`: `Store.RenameDisk` rewrites the disk recorded for a derived object on the renamed disk in the same transaction, so the `Location` readers return the new name.

## Transcoding to a web MP4

`Tool.Transcode(ctx, r, video.Transcode{})` re-encodes any readable video as H.264 and AAC in an MP4 with its index first, so a browser starts playing before the download ends. The display rotation is applied to the pixels, metadata and chapters are dropped, and the picture is scaled down (never up) to fit `MaxWidth`×`MaxHeight` (1920×1080 by default). It returns the output from a work file that is already unlinked; close it when done. Every bound reaches FFmpeg itself, not only the check afterwards: a video longer than `MaxDuration` (30 minutes) is refused from the probe and the encode is cut there with `-t` whatever the container claims, the frame rate is capped (`-fpsmax`, 60) and so is the frame count, the output stops at `MaxOutput` bytes (`-fs`, 2 GiB, then `CodeTooLarge`), and the encode is killed after `Timeout` (30 minutes).

The example's `video/derived.go` reads the stored original back from the disk and writes a 720p web copy beside it:

<!-- ds:block id=webmp4-h4c9w2tn -->
**webMP4** · [`examples/media/video/derived.go:28-42`](../../examples/media/video/derived.go#L28-L42)

```go
func webMP4(ctx context.Context, disk *filesystem.Disk, f records.File) (string, error) {
	src, err := disk.Read(ctx, f.Key)
	if err != nil {
		return "", err
	}
	defer func() { _ = src.Close() }()
	out, err := tool.Transcode(ctx, src, video.Transcode{MaxWidth: webMaxSide, MaxHeight: webMaxSide * 9 / 16})
	if err != nil {
		return "", err
	}
	defer func() { _ = out.Close() }()
	key := f.Key + ".web.mp4"
	_, err = disk.Write(ctx, key, out, filesystem.WriteOptions{ContentType: video.MP4ContentType})
	return key, err
}
```
<!-- /ds:block hash=3b8e1c -->

Check: `go test ./video` in `examples/media` prints `web copy  .web.mp4 (video/mp4)` for the generated clip.

An encode takes a fraction of the video's length on a server core, so run it from a job for anything longer than a short clip. For short clips (a chat's video messages), `tool.TranscodeProcessor(opts)` is an `upload.Policy.Process` that transcodes while the client waits and renames the upload `.mp4`.

### Another codec or container

`Transcode.Profile` replaces H.264 and AAC in MP4 with any encoder pair and muxer FFmpeg has:

```go
vp9 := video.Profile{
	VideoEncoder: "libvpx-vp9", VideoArgs: []string{"-crf", "32", "-b:v", "0"},
	AudioEncoder: "libopus", AudioArgs: []string{"-b:a", "96000"},
	Muxer: "webm", ContentType: "video/webm", Extension: ".webm",
}
proc := tool.TranscodeProcessor(video.Transcode{MaxWidth: 1280, MaxHeight: 720, Profile: &vp9})
```

The processor renames the upload with the profile's extension and type. `CRF`, `Preset` and `AudioBitRate` are libx264 settings and must be zero with a profile; `video.H264Profile(crf, preset, bitRate)` is the default as a profile, a starting point for a hardware H.264 encoder. The box, duration, frame-rate, output-size and metadata bounds still apply: names are checked, the type must be `video/*`, and args that would move a bound (`-t`, `-vf`, `-map`, `-f`, `-i` and the like) are `invalid_argument`. A profile is your configuration, never a client's input. It writes one file; an HLS ladder is `Tool.HLS`, below.

## An HLS ladder on your own storage

`Tool.HLS` encodes a video as an adaptive ladder: several H.264 and AAC renditions in MPEG-TS segments, each with its playlist, and `master.m3u8` naming them. It is opt-in and runs in one FFmpeg process; a platform (next section) is the other model. Every file goes to a writer you give, so the ladder lands on any disk:

```go
dir := "streams/" + f.ID + "/"
err := tool.HLS(ctx, src, video.HLS{
	Renditions: []video.Rendition{
		{Name: "720p", MaxWidth: 1280, MaxHeight: 720, VideoBitRate: 2_800_000},
		{Name: "360p", MaxWidth: 640, MaxHeight: 360, VideoBitRate: 800_000},
	},
}, func(ctx context.Context, name, contentType string, body io.Reader) error {
	_, err := disk.Write(ctx, dir+name, body, filesystem.WriteOptions{ContentType: contentType})
	return err
})
// play dir + video.HLSMaster
```

Nil `Renditions` means `video.DefaultRenditions` (1080p, 720p, 480p, 360p). A rung larger than the video is dropped, except the smallest such rung, which encodes it at its own size: a 720p upload gets 720p, 480p and 360p, never an upscaled 1080p. The writer gets every segment first, then the rendition playlists, then the master, so a player never finds a file missing. Transcode's bounds hold: the input whitelists, the length (`MaxDuration`, `-t`), frame rate and count, the first video and audio streams only, no metadata. `MaxOutput` (4 GiB) bounds all the files together, and FFmpeg is stopped as its work directory grows past it. Only the names the ladder asked for are handed on; anything else FFmpeg wrote fails the call.

With the records plugin, `video.WithHLS(video.HLS{})` writes the ladder for every video into `<video key>.hls-<random>/`, records the master's key in `hls_key`, and treats it like a poster: `WithLocation` places it (name `video.DerivedHLS`), the purge and a replacement delete it after the commit, `MoveFile` moves it, and the orphan sweep keeps it while a row names it. `video.HLSLocation(f)` returns its disk and master key. A failed encode leaves no file behind. The encode runs inside the upload's transaction, so keep the plugin option for short clips and encode long videos from a job, below.

### Another codec, fMP4 segments, AES-128

`HLS.Profile` replaces libx264 and AAC with other encoders, `HLS.SegmentType` picks fragmented MP4 (CMAF) segments over MPEG-TS, and `HLS.Encryption` encrypts every segment with AES-128. All three are off by default, so the ladder above is unchanged. HEVC in HLS needs fMP4, and Apple players need the `hvc1` tag:

```go
err := tool.HLS(ctx, src, video.HLS{
	SegmentType: video.HLSSegmentFMP4,
	Profile: &video.HLSProfile{
		VideoEncoder: "libx265", VideoArgs: []string{"-preset", "fast", "-tag:v", "hvc1"},
		AudioEncoder: "aac", AudioArgs: []string{"-b:a", "128000"},
	},
}, write)
```

An fMP4 ladder adds one `<rendition>_init.mp4` per rendition (typed `video/mp4`) and names its segments `.m4s` (`video/iso.segment`). Each rendition's `VideoBitRate` is still applied after the profile's args. A profile is your configuration, never a client's input: its names are checked, `Preset` and `AudioBitRate` must be zero with it, and args that would move a bound (`-t`, `-vf`, `-map` and the like) or the HLS muxer's own options (`-hls_*`, `-master_pl_name`, `-var_stream_map`, `-method`, `-http_*`) are `invalid_argument`, so a profile cannot write outside the work directory.

For AES-128, give the 16-byte key and the URI players fetch it from:

```go
key := make([]byte, video.HLSKeyBytes)
_, _ = rand.Read(key) // store it where your key endpoint reads it
opts := video.HLS{Encryption: &video.HLSEncryption{Key: key, KeyURI: "https://app.example/keys/" + f.ID}}
```

The key never reaches the writer: FFmpeg reads it from a work directory of its own, removed before `HLS` returns, and the playlists carry only `KeyURI` (at most 2048 bytes, no control character or `"`). Storing the key and serving `KeyURI` only to viewers entitled to the video is your part; a key served beside the segments protects nothing. `HLSEncryption` prints and logs without its key. FFmpeg encrypts MPEG-TS segments only, so `Encryption` with `HLSSegmentFMP4` is `unsupported`.

### Ladders made in a job

`plugin.EncodeHLS(ctx, store, scope, fileID, opts)` reads the file, encodes its ladder outside any transaction, and records it: run it from a job for videos too long to encode inside the upload. The ladder then follows every rule of one `WithHLS` made: placed by `WithLocation`, deleted by the purge and by a replacement after the commit, moved by `MoveFile`, kept by the orphan sweep while the row names it. A ladder the file had before is deleted once the new one commits. A failed encode or attach deletes what it wrote.

To encode elsewhere (another worker, another tool), take the place from the plugin and record it with `AttachHLS`:

```go
l, err := plugin.NewHLSLadder(f) // the Location's disk, a fresh directory, f's SHA-256
w, err := l.Writer(store)
err = tool.HLS(ctx, src, opts, w)
f, err = plugin.AttachHLS(ctx, store, scope, f.ID, l)
```

`AttachHLS` locks the row and accepts only a ladder where `NewHLSLadder` could have put it (the Location's disk, one fresh directory under its prefix), since the purge deletes the whole directory: anything else is `invalid_argument`, so attaching can never make a purge delete objects that are not the ladder's. The master playlist must exist (`not_found`), the file must be a live, released video, and `SourceSHA256` must still be the file's SHA-256 (`conflict` when the content was replaced during the encode). Attaching the ladder the file already has changes nothing, and a rolled-back attach deletes the ladder it would have recorded.

Try it: [`examples/media/video`](../../examples/media/video) runs this section in `hls.go` ([`hlsLadder`](ds:block?id=hlsladder-b8r4w6ny)): `go run ./video clip.mp4` from `examples/media` (it needs FFmpeg), then look for `hls       master.m3u8 (application/vnd.apple.mpegurl) names 360p.m3u8, 180p.m3u8`.

## Storyboards for scrubbing

`Tool.Storyboard(ctx, r, video.Storyboard{})` returns one JPEG holding a grid of frames taken every `Interval` (10 seconds) along the video, each `TileSize` pixels on its longest side (160), plus where each tile sits and the span it stands for. `Sprite.WebVTT(url)` writes the thumbnails track players read (Video.js, Plyr, Vidstack, JW Player): one cue per tile pointing at `url#xywh=x,y,w,h`. The sprite stays bounded whatever the video: at most `MaxTiles` tiles (100; a longer video gets a wider interval, never a bigger image), tiles of at most 512 pixels, one ffmpeg run under `Tool.Timeout`, the JPEG capped by `Tool.MaxPoster`.

<!-- ds:block id=storyboard-p6t3m8xk -->
**storyboard** · [`examples/media/video/derived.go:48-55`](../../examples/media/video/derived.go#L48-L55)

```go
func storyboard(ctx context.Context, disk *filesystem.Disk, f records.File) (video.Sprite, error) {
	src, err := disk.Read(ctx, f.Key)
	if err != nil {
		return video.Sprite{}, err
	}
	defer func() { _ = src.Close() }()
	return tool.Storyboard(ctx, src, video.Storyboard{Interval: storyboardEvery, Columns: 3, TileSize: 120})
}
```
<!-- /ds:block hash=e473da -->

Store `sprite.JPEG`, then serve the track beside the player:

```go
vtt, err := sprite.WebVTT("/media/" + id + "/storyboard.jpg")
```

Check: for the three-second 640×360 test clip, with a tile every half second, the example prints `storyboard 6 tiles of 120x68, 3 per row, last 2.5s-3s at 240,68`: six tiles, each 120 pixels on its longest side, the last one in the third column of the second row.

## Adaptive streaming through Mux or Cloudflare Stream

Running a ladder yourself (above) means encoding, storing and serving every rendition; most products hand it to a platform instead. `contrib/videoplatform` is a records plugin that does that for any `videoplatform.Platform`; `contrib/videoplatform/mux` and `contrib/videoplatform/cfstream` are the adapters, over each platform's REST API with no vendor SDK. `plugin.Publish(ctx, store, scope, fileID)` hands the platform a signed URL of the file's object (valid for an hour, `WithSourceTTL`), records the asset in `fs_video_assets` and sets the file's status to `processing`. The platform calls `plugin.Webhook(store, logger)`, which checks the request's signature (HMAC-SHA256 over the timestamp and body, refused when older than five minutes) before it reads anything (a correctly signed body it cannot parse is 400; the same request replayed within those five minutes changes nothing), then records the asset's state, sets the file `ready` or `failed` and emits `videoplatform.ready` or `videoplatform.failed`. `plugin.Asset` returns the HLS and thumbnail URLs for the page; `plugin.Sync` asks the platform directly, for a webhook that never came. Purging or replacing the file, or `Unpublish`, deletes the asset on the platform after the change commits.

The example [`examples/media/streaming`](../../examples/media/streaming) runs the whole flow against a fake Mux API served in the same process (`fakemux.go` answers the three calls the adapter makes), so it needs no account and no FFmpeg. Four steps:

**1. The adapter.** `mux.New` takes the API token, the webhook signing secret and the playback policy (`mux.PolicyPublic`, or `PolicySigned` for signed playback URLs). Credentials are `videoplatform.Secret` values, which print as a redaction in `%v`, `%+v` and `slog`. `BaseURL` points the example at the fake; leave it empty in production:

<!-- ds:block id=newplatform-m6q2x8vd -->
**newPlatform** · [`examples/media/streaming/main.go:59-67`](../../examples/media/streaming/main.go#L59-L67)

```go
func newPlatform(baseURL string) (*mux.Client, error) {
	return mux.New(mux.Config{
		TokenID:       "mux-token-id",
		TokenSecret:   tokenSecret,
		WebhookSecret: webhookSecret,
		Policy:        mux.PolicyPublic,
		BaseURL:       baseURL,
	})
}
```
<!-- /ds:block hash=92c1f9 -->

**2. The plugin.** Install `videoplatform.New(platform)` like any plugin. The disk must be able to sign URLs (presign or `WithURLSigner`), because the platform downloads the video from one; `WithSourceURL` replaces how that URL is made (a CDN in front, a public bucket):

<!-- ds:block id=newstore-z3k9w5tf -->
**newStore** · [`examples/media/streaming/main.go:72-99`](../../examples/media/streaming/main.go#L72-L99)

```go
func newStore(ctx context.Context, platform videoplatform.Platform) (*records.Store, *videoplatform.Plugin, error) {
	signer, err := signedurl.New(signBase, []signedurl.Key{{ID: "k1", Secret: signingKey}})
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskMedia, memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1) // one connection keeps the in-memory database alive
	plugin := videoplatform.New(platform, videoplatform.WithSourceTTL(time.Hour))
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(plugin))
	if err != nil {
		return nil, nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return store, plugin, nil
}
```
<!-- /ds:block hash=f3cc02 -->

**3. Publish.** Upload as usual, then hand the file to the platform. `Publish` refuses a file that is not a video (`invalid_argument`), a [quarantined](../records/processing-status.md#quarantine-until-scanned) one, and one already published (`exists`), so a retry never bills a second asset:

<!-- ds:block id=publish-c4n8r2hy -->
**publish** · [`examples/media/streaming/main.go:108-115`](../../examples/media/streaming/main.go#L108-L115)

```go
func publish(ctx context.Context, s *records.Store, p *videoplatform.Plugin, body io.Reader) (records.File, videoplatform.Published, error) {
	f, err := s.Upload(ctx, scope, diskMedia, body, "clip.mp4", videos)
	if err != nil {
		return records.File{}, videoplatform.Published{}, err
	}
	v, err := p.Publish(ctx, s, scope, f.ID)
	return f, v, err
}
```
<!-- /ds:block hash=47681b -->

**4. The webhook.** Mount the handler at the URL you register with the platform:

<!-- ds:block id=mountwebhook-t7v3m9kq -->
**mountWebhook** · [`examples/media/streaming/main.go:121-123`](../../examples/media/streaming/main.go#L121-L123)

```go
func mountWebhook(mx *http.ServeMux, s *records.Store, p *videoplatform.Plugin) {
	mx.Handle("POST /hooks/mux", p.Webhook(s, nil))
}
```
<!-- /ds:block hash=4cb41c -->

Check: `cd examples/media && go run ./streaming` prints:

```text
published a1b2c3 on mux: preparing, fetched from a signed URL true
file status: processing
webhook: 204
forged webhook: 403
asset ready, file ready, 12.5s
hls https://stream.mux.com/pb9x8y7.m3u8
thumbnail https://image.mux.com/pb9x8y7/thumbnail.jpg
```

The webhook signed with the right secret is accepted (204) and marks the asset and the file ready; the same body signed with another secret is refused (403) and changes nothing. Cloudflare Stream is `cfstream.New(cfstream.Config{...})` in step 1; nothing else changes.

The module's tests run against in-process fakes of each documented API. What needs a real account: the credentials, the platform fetching your signed URL (the bucket must be reachable from the internet), and webhooks reaching a public URL.

### Platform options

Each adapter's `Config.CreateOptions` adds fields to the create request, as raw JSON under the platform's own names:

```go
client, err := mux.New(mux.Config{
	TokenID: id, TokenSecret: videoplatform.NewSecret(secret), WebhookSecret: videoplatform.NewSecret(hook),
	CreateOptions: map[string]json.RawMessage{
		"video_quality": json.RawMessage(`"basic"`),
		"mp4_support":   json.RawMessage(`"capped-1080p"`),
	},
})
```

For Cloudflare Stream, `{"maxDurationSeconds": 600}`, `{"allowedOrigins": ["app.example.com"]}` or a watermark profile. The fields the adapter sets itself (Mux `inputs`, `playback_policies`, `passthrough`; Stream `url`, `meta`, `requireSignedURLs`) and values that are not JSON are refused by `New`.

Those options are the same for every file. For options that depend on the file (a plan's resolution tier, a creator's watermark, a tenant's allowed origins), `videoplatform.WithCreateOptions` returns fields per published file, merged over `Config.CreateOptions` (the file's value wins a field both set):

```go
plugin := videoplatform.New(client, videoplatform.WithCreateOptions(
	func(ctx context.Context, s *records.Store, f records.File) (map[string]json.RawMessage, error) {
		tier, err := plans.ResolutionTier(ctx, f.Scope) // your plan lookup
		if err != nil {
			return nil, err
		}
		return map[string]json.RawMessage{"max_resolution_tier": json.RawMessage(strconv.Quote(tier))}, nil
	}))
```

The function runs before the platform is called: an error from it, a reserved field or a value that is not JSON fails `Publish` with no asset created and nothing recorded. Without the option every file gets the `Config`'s request, as before. A `videoplatform.Source` passed to `Ingest` directly carries the same fields in `Source.Options`.

## Options

| Option | What it does |
|---|---|
| `Tool.FFprobe`, `Tool.FFmpeg` | program paths or names; empty means `ffprobe` and `ffmpeg` on `PATH` |
| `Tool.Timeout` | per program run; 0 means one minute; a run cut short is `CodeUnavailable` |
| `Tool.MaxInput` | bytes copied to the work file; 0 means 2 GiB; more is `CodeTooLarge` |
| `Tool.MaxPoster` | a poster's encoded size; 0 means 10 MiB |
| `Tool.Formats` | the demuxers FFmpeg may use; nil means `video.DefaultFormats` |
| `Tool.TempDir` | where work files go; empty means the system's temp directory; they are removed before each call returns |
| `video.Limits{MaxDuration, MaxWidth, MaxHeight}` | what `Inspect` accepts; zero fields do not limit. `MaxDuration` is checked against the container's claim and against the packets' timestamps (one more ffprobe pass), so a header patched to say 1 second over 20 seconds of video is refused, and so is a video whose packets carry no timestamps |
| `video.WithPoster(at)` | the plugin also stores a poster frame |
| `video.WithHLS(opts)` | the plugin also writes an HLS ladder per video (see above); off by default |
| `video.HLS{Renditions, SegmentDuration, MaxDuration, MaxFrameRate, MaxOutput, Timeout, Preset, AudioBitRate}` | `Tool.HLS`'s ladder (`video.DefaultRenditions`, at most 8), segment length (6 s, 1 s to 1 min), length (30 minutes), frame rate (60), all files' size (4 GiB), time (30 minutes), x264 preset (`veryfast`) and AAC rate (128 kb/s) |
| `video.HLS{Profile, SegmentType, Encryption}` | other encoders (`video.HLSProfile`; nil is H.264 and AAC), `video.HLSSegmentFMP4` segments (`""` is MPEG-TS), AES-128 (`video.HLSEncryption{Key, KeyURI, IV}`; MPEG-TS only); all off by default |
| `plugin.EncodeHLS`, `plugin.NewHLSLadder`, `HLSLadder.Writer`, `plugin.AttachHLS` | a ladder made in a job, recorded so it follows the `WithHLS` rules (see above) |
| `video.WithLocation(fn)` | where posters and ladders go: `fn(f, name)` (name `video.DerivedPoster` or `video.DerivedHLS`) returns a disk name (`""` for the video's own) and a key prefix; the plugin appends `<random>.jpg` or `<random>/`. An error fails the upload with nothing written; nil keeps the default, beside the video |
| `video.WithStrict()` | the plugin refuses a video file ffprobe cannot read, instead of recording it without metadata |
| `video.Transcode{MaxWidth, MaxHeight, MaxDuration, MaxFrameRate, MaxOutput, Timeout, CRF, Preset, AudioBitRate}` | `Transcode`'s box (1920×1080), length (30 minutes), frame rate (60), output size (2 GiB), time (30 minutes), quality (CRF 23), x264 preset (`veryfast`, one of `video.Presets`) and AAC rate (128 kb/s) |
| `video.Storyboard{Interval, Columns, TileSize, MaxTiles}` | the storyboard's spacing (10 s), tiles per row (10), tile size (160, at most 512) and tile count (100, at most 400) |
| `videoplatform.WithSourceTTL(d)`, `videoplatform.WithSourceURL(fn)` | how long the platform's download URL lives (one hour), or how it is made (by default `Store.URL`) |
| `videoplatform.WithCreateOptions(fn)` | create-request fields per published file, merged over `Config.CreateOptions`; nil sends the `Config`'s request |
| `videoplatform.NewSecret(s)` | wraps the API token and webhook secret in each `Config`; a logged `Config` or client shows `[redacted]` |
| `mux.Config{Policy}`, `cfstream.Config{RequireSignedURLs}` | play publicly or only with a token your app signs |

## Pitfalls

- **A missing FFmpeg fails every video upload.** The plugin and `Inspect` return an error wrapping `video.ErrNoProgram` (`CodeUnavailable`) rather than recording videos without metadata; check for it at start-up with a probe of a small file if you deploy to new machines.
- **The work happens inside the upload.** Probing and the poster run in the records transaction, and `Sanitize` and `Inspect` in the request; a large video holds the request for as long as FFmpeg needs. For long videos, record the file with `records.StatusProcessing` and describe it from a job, and make the ladder with `EncodeHLS` there.
- **`width`, `height`, `duration_ms` and `audio_codec` are shared keys.** The dimensions plugin writes the first two for images, contrib/audio the last two for audio files. Each plugin sets and clears them only on its own type, so they never wipe each other's values in any install order; when a video is replaced by an image or an audio file, the plugin for the new type sets the shared keys, and without it installed they keep the video's values.
- **`poster_key`, `poster_disk`, `hls_key` and `hls_disk` are the plugin's alone.** They are dropped from the metadata you pass for a new file of any type, because the purge deletes the object they name and the orphan sweep keeps it.
- **Sanitize keeps video and audio only.** Subtitles and timed metadata tracks are dropped with the rest of the metadata, on purpose: phones store the location in a metadata track too.
- **A webhook for an asset no row knows is acknowledged.** It may be an asset made outside the app, or one that raced `Publish`'s commit; run `plugin.Sync` for files still `processing` after a while (a job) so a lost event cannot leave a video waiting forever.
- **Presigned and Uppy uploads skip `Process`.** They go straight to the bucket; `Confirm` runs `Inspect`, but metadata stays in those files. Route videos that must be cleaned through a form or tus upload.

## Quarantined files

A [quarantined](../records/processing-status.md#quarantine-until-scanned) file is not given to ffprobe and gets no poster when it is recorded or replaced; it is described, with its poster, when its scan passes (`BeforeRelease`). `videoplatform`'s `Publish` refuses a quarantined file (`records.ErrQuarantined`) before calling the platform.

## Next

- [Audio](audio.md): durations, waveforms and voice-note transcoding with the same runner.
- [PDF previews](pdf.md) and [SVG sanitising](svg.md): the other untrusted formats.
- [Image dimensions](dimensions.md) and [variants](variants.md): the same for images.
