# Audio metadata, waveforms and voice notes

You will have every uploaded audio file checked and described: refused when it is not readable audio or is too long, stored with its duration, codec, bit rate, sample rate and channels on its file row, optionally with the peaks a player draws as bars, and, for voice notes, transcoded to Opus or AAC on the way in as chat apps do. The package is `contrib/audio`, which runs FFmpeg's `ffprobe` and `ffmpeg` through the same bounded runner as [video](video.md) (`contrib/video/ffmpeg`).

## Before you start

- FFmpeg installed where the server runs, found on `PATH` or set as `audio.Tool.FFprobe` and `audio.Tool.FFmpeg`.
- `go get github.com/ubgo/filesystem/contrib/audio`. It is its own module because it runs external programs.
- [Uploads](../concepts/uploads.md) explains `Process` and `Inspect`; [files as records](../records/README.md) explains plugins.
- The example is [`examples/media/audio`](../../examples/media/audio), in the media module: `cd examples/media && go run ./audio` uploads a two-second note generated in code and prints every step below; `go test ./audio` runs them, and skips when FFmpeg is missing.

When to use what: transcode on upload (step 1) for voice notes and clips of a minute or two, where the client can wait; record long audio (podcasts, lectures) as it is and transcode from a job (step 4). The plugin (step 2) is worth installing whenever you list audio files, because a duration and a waveform are what a player shows before the first byte is fetched.

## What it guards against

Every run is bounded the way video's are: at most `Tool.MaxInput` bytes (500 MiB) copied to a work file, each run killed after `Tool.Timeout` (five minutes), output capped, FFmpeg limited to the demuxers in `Tool.Formats` and the `file` protocol, no shell. Work sized by the length is bounded by `Tool.MaxDuration` (three hours): longer audio is refused from the probe, and decoding is cut there with `-t` and capped at that many samples whatever the container claims, so a file that lies about its length cannot run longer. A file FFmpeg cannot read is `CodeRejected` with a message that names no server path (`audio: ffprobe could not read the file`).

## Step 1 — The upload policy

`Inspect` probes every stored audio upload and refuses it (`rejected`, the object deleted) when ffprobe cannot read it as audio or it is longer than `Limits.MaxDuration`. `TranscodeProcessor` re-encodes what it accepts before it is stored and renames it: Opus in Ogg (`audio/ogg`, `.ogg`, 48 kb/s by default) for voice notes, or AAC in MP4 (`audio/mp4`, `.m4a`, 128 kb/s) for players that lack Opus. Metadata, cover art and any video stream are dropped. Other content types pass both untouched, so the policy composes with others.

<!-- ds:block id=voicepolicy-k5w9c3nd -->
**policy** · [`examples/media/audio/main.go:56-62`](../../examples/media/audio/main.go#L56-L62)

```go
var policy = upload.Policy{
	MaxSize: 25 << 20,
	Allowed: []string{"audio/*"},
	Key:     upload.RandomKey("notes"),
	Process: tool.TranscodeProcessor(audio.Transcode{Format: audio.Opus, Mono: true}),
	Inspect: tool.Inspect(audio.Limits{MaxDuration: maxNote}),
}
```
<!-- /ds:block hash=1bc415 -->

The duration is checked twice: against the container's claim and against the packets' timestamps (one more ffprobe pass), because the claim is a header a client can patch. Audio whose packets carry no timestamps is refused.

## Step 2 — The records plugin

`audio.NewPlugin(tool, options...)` stores what ffprobe reports on every audio file's row when it is recorded and when its content is replaced. `WithWaveform(points)` also stores that many peaks, so a player draws the bars without fetching the audio; `WithStrict()` refuses unreadable audio instead of recording it without metadata:

<!-- ds:block id=newstore-r8m3x6tj -->
**newStore** · [`examples/media/audio/main.go:67-90`](../../examples/media/audio/main.go#L67-L90)

```go
func newStore(ctx context.Context) (*records.Store, error) {
	disk, err := filesystem.NewDisk(diskMedia, memory.New())
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one connection keeps the in-memory database alive
	plugin := audio.NewPlugin(tool, audio.WithWaveform(waveformPoints), audio.WithStrict())
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(plugin))
	if err != nil {
		return nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, err
	}
	return store, nil
}
```
<!-- /ds:block hash=347b59 -->

The plugin stores at most `MaxStoredWaveformPoints` (1024) peaks, one byte each in base64, because they ride along with every read of the row.

## Step 3 — Read what was recorded

`audio.Of(f)` returns the recorded `Meta` (duration, codec, bit rate, sample rate, channels) and `audio.WaveformOf(f)` the peaks in `[0, 1]`; each reports `ok == false` for a file without them (not audio, unreadable, or recorded before the plugin was installed):

<!-- ds:block id=describe-q2t7v4hm -->
**describe** · [`examples/media/audio/main.go:94-107`](../../examples/media/audio/main.go#L94-L107)

```go
func describe(w io.Writer, f records.File) error {
	m, ok := audio.Of(f)
	if !ok {
		return fmt.Errorf("no audio metadata recorded for %s", f.ID)
	}
	peaks, ok := audio.WaveformOf(f)
	if !ok {
		return fmt.Errorf("no waveform recorded for %s", f.ID)
	}
	fmt.Fprintf(w, "stored   %s (%s)\n", f.Filename, f.ContentType)
	fmt.Fprintf(w, "audio    %s, %s, %d Hz, %d channel(s)\n", m.Duration.Round(100*time.Millisecond), m.Codec, m.SampleRate, m.Channels)
	fmt.Fprintf(w, "waveform %d peaks, loudest %.1f\n", len(peaks), maxOf(peaks))
	return nil
}
```
<!-- /ds:block hash=b9be6c -->

Check: the example prints `stored   note.ogg (audio/ogg)`: the WAV upload was stored as Opus and renamed. Then `audio    2s, opus, 48000 Hz, 1 channel(s)` and `waveform 32 peaks, loudest 0.8` (the tone was generated at 80% of full scale).

## Step 4 — Transcode outside an upload

`Tool.Transcode(ctx, r, opts)` does what `TranscodeProcessor` does, for a job: here an AAC copy of a stored note, for Safari before 17 and iOS players. It returns the output from a work file that is already unlinked; close it when done. `Tool.Waveform(ctx, r, points)` and `Tool.Probe(ctx, r)` work the same way without the plugin. `Waveform` folds samples into peaks as FFmpeg writes them, so memory is the size of the result whatever the length.

<!-- ds:block id=toaac-w4h9n2cp -->
**toAAC** · [`examples/media/audio/main.go:113-129`](../../examples/media/audio/main.go#L113-L129)

```go
func toAAC(ctx context.Context, s *records.Store, f records.File) (audio.Info, error) {
	disk, err := s.Disks().Disk(f.Disk)
	if err != nil {
		return audio.Info{}, err
	}
	src, err := disk.Read(ctx, f.Key)
	if err != nil {
		return audio.Info{}, err
	}
	defer func() { _ = src.Close() }()
	out, err := tool.Transcode(ctx, src, audio.Transcode{Format: audio.AAC})
	if err != nil {
		return audio.Info{}, err
	}
	defer func() { _ = out.Close() }()
	return tool.Probe(ctx, out)
}
```
<!-- /ds:block hash=afa820 -->

Check: the example prints `aac copy aac in mov,mp4,m4a,3gp,3g2,mj2` (ffprobe's name for the MP4 family), then uploads a file that starts like a WAV but holds no audio and prints `broken.wav: rejected`.

### Another encoding

`Transcode.Profile` replaces the built-in formats, for MP3 or FLAC:

```go
mp3 := audio.Profile{Encoder: "libmp3lame", Muxer: "mp3", BitRate: 128_000, ContentType: "audio/mpeg", Extension: ".mp3"}
rc, err := tool.Transcode(ctx, r, audio.Transcode{Profile: &mp3, Mono: true})
```

`Format` must be empty with a profile; `Mono` and `BitRate` still apply (a profile's `BitRate` 0 leaves it to the encoder). Names are checked, the type must be `audio/*`, and args that would move the duration bound or the stream selection (`-t`, `-map`, `-f` and the like) are `invalid_argument`. `Transcode.ContentType()` and `Extension()` name the output, and `TranscodeProcessor` renames uploads with them.

## Try it

```sh
cd examples/media
go run ./audio
go test ./audio
```

```text
stored   note.ogg (audio/ogg)
audio    2s, opus, 48000 Hz, 1 channel(s)
waveform 32 peaks, loudest 0.8
aac copy aac in mov,mp4,m4a,3gp,3g2,mj2
broken.wav: rejected
```

The test `TestTooLong` uploads a note one second over the five-minute limit and expects `rejected`.

## Options

| Option | What it does |
|---|---|
| `Tool.FFprobe`, `Tool.FFmpeg`, `Tool.Formats`, `Tool.TempDir` | programs, allowed demuxers (`audio.DefaultFormats`) and work directory |
| `Tool.Timeout`, `Tool.MaxInput`, `Tool.MaxDuration` | per run (five minutes), input bytes (500 MiB), length for waveforms and transcodes (three hours) |
| `audio.Transcode{Format, BitRate, Mono, MaxOutput}` | `audio.Opus` (default) or `audio.AAC`, the bit rate, mixing to one channel, output bytes (500 MiB) |
| `audio.Limits{MaxDuration}` | what `Inspect` accepts; checked against the container's claim and the packets' timestamps (one more ffprobe pass), so a patched header cannot slip long audio through, and audio whose packets carry no timestamps is refused |
| `audio.WithWaveform(points)`, `audio.WithStrict()` | the plugin also stores peaks; refuses unreadable audio instead of recording it without metadata |

## Pitfalls

- **A missing FFmpeg fails every audio upload** with an error wrapping `audio.ErrNoProgram` (`CodeUnavailable`), rather than recording files without metadata.
- **Transcoding on upload holds the request.** Fine for voice notes of a minute or two; record long recordings as they are and transcode from a job.
- **With `WithWaveform`, audio over `Tool.MaxDuration` gets no metadata at all**, since the waveform cannot be made; raise the limit or leave the waveform to a job.
- **`Allowed: []string{"audio/*"}` is judged from the bytes.** A file named `.mp3` that holds something else is refused by type before FFmpeg sees it; one that starts like audio but does not decode is refused by `Inspect`.
- **No libopus in the FFmpeg build** makes every Opus transcode fail. Check `ffmpeg -encoders | grep opus` on the server, or use `audio.AAC`, which FFmpeg's built-in encoder writes.
- **`duration_ms` and `audio_codec` are shared with contrib/video.** Each plugin sets and clears them only on its own type, so neither wipes the other's values in any install order; an application's own keys on other files are left alone too.
- **Different FFmpeg versions report slightly different values** (bit rates, the container name). Store and compare durations and codecs, not exact bit rates.

## Quarantined files

A [quarantined](../records/processing-status.md#quarantine-until-scanned) file is not given to ffprobe when it is recorded or replaced; it is described, with its waveform, when its scan passes (`BeforeRelease`).

## Next

- [Video](video.md): metadata, posters, transcodes, storyboards and streaming platforms.
