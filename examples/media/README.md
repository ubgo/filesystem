# Media examples: video, audio, streaming, PDF and SVG

Uploads of the untrusted formats that need more than sniffing: videos through `contrib/video` (FFmpeg), voice notes through `contrib/audio` (FFmpeg), PDFs through `contrib/pdf` (Poppler) and SVG through `contrib/svg` and `contrib/svg/raster` (pure Go), plus adaptive streaming through a hosted platform with `contrib/videoplatform`. Each program uploads one file the way an application would and prints what was stored. This directory is a separate Go module (`github.com/ubgo/filesystem/examples/media`) so the main examples module does not depend on the media plugins.

## Run it

```sh
cd examples/media
go run ./video clip.mp4     # needs ffmpeg and ffprobe
go run ./pdf report.pdf     # needs pdfinfo and pdftoppm
go run ./svg logo.svg
go run ./audio              # needs ffmpeg and ffprobe; the note is generated
go run ./streaming          # a fake Mux API in the same process; no account
go test ./...               # video, audio and pdf skip, saying so, when their programs are missing
```

Everything runs in memory (a memory disk, the records tables in in-memory SQLite) and is gone when the program exits.

| Program | What it shows | Guide |
|---|---|---|
| `video/` | `Sanitize` removing a phone video's location, `Inspect` limiting duration and size, the records plugin storing duration, display size, codecs and a poster, then a web MP4 copy (`Transcode`), a storyboard sprite for scrubbing (`Storyboard`) and an HLS ladder on the same disk (`HLS`) | [video](../../docs/plugins/video.md) |
| `audio/` | `Inspect` limiting a voice note's duration, `TranscodeProcessor` storing it as mono Opus, the records plugin storing duration, codec, sample rate and a waveform, then an AAC copy | [audio](../../docs/plugins/audio.md) |
| `streaming/` | `videoplatform` with the Mux adapter: `Publish` hands the video over through a signed URL, the signed webhook marks it ready, a forged one is refused, and the HLS and thumbnail URLs come back | [video](../../docs/plugins/video.md#adaptive-streaming-through-mux-or-cloudflare-stream) |
| `pdf/` | `Inspect` limiting pages, the records plugin storing the page count, page size and a first-page preview | [PDF](../../docs/plugins/pdf.md) |
| `svg/` | `svg.Policy` accepting SVG and nothing else active, the sanitiser's report, the stored document, and a PNG thumbnail drawn by `contrib/svg/raster` | [SVG](../../docs/plugins/svg.md) |

## Walkthrough

```sh
go run ./video phone.mp4
go run ./pdf report.pdf
go run ./svg logo.svg
```

```text
stored    videos/svkjb4mapmtwnt6qdjgnzlpmor.mp4 (video/mp4, 100413 bytes)
duration  4s
size      720x1280
codecs    h264 / aac
poster    videos/svkjb4mapmtwnt6qdjgnzlpmor.mp4.poster-g2b6tx7pbe.jpg (image/jpeg, 24477 bytes)
web copy  .web.mp4 (video/mp4)
storyboard 8 tiles of 68x120, 3 per row, last 3.5s-4s at 68,240
stored    documents/n2jpezfkd4rzrlqlz7elvpwosu.pdf (application/pdf, 3577 bytes)
pages     3
page 1    595x842 pt
preview   documents/n2jpezfkd4rzrlqlz7elvpwosu.pdf.preview-db7kqww75l.jpg (image/jpeg, 4731 bytes)
removed   attribute onload
removed   element script
removed   element a
removed   element foreignObject
removed   attribute href
stored    logos/4nzfxd5cc3n4a6skat2mmbhbe4.svg (image/svg+xml, 187 bytes)
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="64" height="64">
  
  <rect width="64" height="64" fill="#2563eb"></rect>
  
  <use></use>
</svg>
raster    128x128 image/png
```

The video program ends with an HLS ladder on the same disk (`video/hls.go`): [`hlsLadder`](ds:block?id=hlsladder-b8r4w6ny) hands `Tool.HLS` the app's three rungs (720p, 360p, 180p) and a writer that stores each file under `streams/<file ID>/`. For the 640x360 clip the test generates (`ffmpeg -f lavfi -i testsrc=size=640x360:rate=30:duration=3 -f lavfi -i sine=duration=3 -c:v libx264 -pix_fmt yuv420p -c:a aac -shortest clip.mp4`), it prints:

```text
hls       master.m3u8 (application/vnd.apple.mpegurl) names 360p.m3u8, 180p.m3u8
hls       2 segments, then 3 playlists, the master last: true
```

The 720p rung is larger than the clip, so it is dropped rather than upscaled; the master names the two rungs that fit. The writer receives every segment first, then the rendition playlists, then `master.m3u8`, so a player given the master never finds a file missing. The ladder is real FFmpeg output, so its test skips, saying so, when `ffmpeg` or `ffprobe` is not installed.

```sh
go run ./audio
go run ./streaming
```

```text
stored   note.ogg (audio/ogg)
audio    2s, opus, 48000 Hz, 1 channel(s)
waveform 32 peaks, loudest 0.8
aac copy aac in mov,mp4,m4a,3gp,3g2,mj2
broken.wav: rejected
published a1b2c3 on mux: preparing, fetched from a signed URL true
file status: processing
webhook: 204
forged webhook: 403
asset ready, file ready, 12.5s
hls https://stream.mux.com/pb9x8y7.m3u8
thumbnail https://image.mux.com/pb9x8y7/thumbnail.jpg
```

## Files

| File | What it holds |
|---|---|
| `go.mod` | The separate module, with `replace` directives back to the library's modules |
| `video/main.go`, `video/derived.go`, `video/hls.go`, `video/main_test.go` | The video upload, the web copy, the storyboard and the HLS ladder; the test generates a clip with ffmpeg |
| `audio/main.go`, `audio/main_test.go` | The voice note upload and the AAC copy; the note is a tone generated in code, and the test checks the duration limit |
| `streaming/main.go`, `streaming/fakemux.go`, `streaming/main_test.go` | Publishing to Mux and the webhook; `fakemux.go` answers the three Mux API calls the adapter makes |
| `pdf/main.go`, `pdf/main_test.go` | The PDF upload; the test builds a two-page document |
| `svg/main.go`, `svg/raster.go`, `svg/main_test.go` | The SVG upload and its raster thumbnail; the test uses an attacker's logo |
