# contrib/audio

`github.com/ubgo/filesystem/contrib/audio` (package `audio`) reads and converts uploaded audio with FFmpeg's programs: `ffprobe` for the duration, codec, bit rate, sample rate and channels, and `ffmpeg` for waveform peaks and for transcoding to Opus or AAC. On top of that: an `upload.Inspector` that refuses unreadable or over-long audio, an `upload.Processor` that transcodes voice notes on upload, and a records plugin that stores the metadata, and optionally the waveform, with every audio file. Every run goes through the bounded runner of `contrib/video/ffmpeg`; the module imports nothing but the standard library and the library's own packages (`deps_test.go` enforces it).

```sh
go get github.com/ubgo/filesystem/contrib/audio
```

Needs `ffprobe` and `ffmpeg` on `PATH` (or configured paths). The guide: [audio](../../docs/plugins/audio.md).

## API

| Name | What it is |
|---|---|
| `Tool` | runs the programs; the zero value uses the defaults. Fields: `FFprobe`, `FFmpeg`, `Timeout`, `MaxInput`, `MaxDuration`, `Formats`, `TempDir` |
| `Tool.Probe(ctx, r) (Info, error)` | `Info{Format, Codec, Duration, BitRate, SampleRate, Channels}`; a file without an audio stream is `CodeRejected` |
| `Tool.Waveform(ctx, r, points) ([]float64, error)` | peaks in [0, 1], 1 to `MaxWaveformPoints`; audio over `MaxDuration` or of unknown length is `CodeRejected` |
| `Tool.Transcode(ctx, r, Transcode) (io.ReadCloser, error)` | `Transcode{Format, BitRate, Mono, MaxOutput}` with `Opus` (Ogg) or `AAC` (MP4); `Format.ContentType()`, `Format.Extension()`, `FormatValues` |
| `Transcode.Profile *Profile`, `Profile{Encoder, Muxer, BitRate, Args, ContentType, Extension}`, `Transcode.ContentType()`, `Extension()` | another encoding than `Opus` or `AAC`, such as MP3 (`libmp3lame`, `mp3`) or FLAC; `Format` must be empty with it, `Mono` and `BitRate` still apply. Names are checked, the type must be `audio/*`, and args that would move a safety bound are `invalid_argument` |
| `Tool.TranscodeProcessor(Transcode) upload.Processor`, `Tool.Inspect(Limits) upload.Inspector` | the upload hooks; other types pass. `Limits.MaxDuration` is checked against the container's claim and the packets' timestamps |
| `NewPlugin(Tool, ...PluginOption) *Plugin`, `WithWaveform(points)`, `WithStrict()` | the records plugin (`PluginName` `audio`) |
| `Of(records.File) (Meta, bool)`, `WaveformOf(records.File) ([]float64, bool)` | read what the plugin recorded |
| `MetaDuration`, `MetaCodec`, `MetaBitRate`, `MetaSampleRate`, `MetaChannels`, `MetaWaveform` | the metadata keys; at most `MaxStoredWaveformPoints` points are stored |
| `IsAudio(contentType) bool`, `ErrNoProgram`, `ProgramError` | helpers and errors, shared with `contrib/video/ffmpeg` |
| `DefaultTimeout`, `DefaultMaxInput`, `DefaultMaxDuration`, `DefaultMaxOutput`, `DefaultFormats` | the defaults |

## Shared metadata keys

`duration_ms` and `audio_codec` are contrib/video's keys too (a video's audio track). The plugin therefore sets and clears its keys only on audio files: a video or an application's own file keeps them, whichever order the plugins are installed in. When an audio file's content is replaced by another type, its keys are dropped, except those two when the new content is a video, which contrib/video sets; without it installed they keep the audio file's values.

## Limits

- Every call copies the audio to a work file; budget disk space in `TempDir`.
- Tests skip only when `ffmpeg` or `ffprobe` is not installed, and say so.

## Quarantined files

A [quarantined](../../docs/records/processing-status.md#quarantine-until-scanned) file is not given to ffprobe when it is recorded or replaced; it is described, with its waveform, when its scan passes (`BeforeRelease`).
