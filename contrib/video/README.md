# contrib/video

`github.com/ubgo/filesystem/contrib/video` (package `video`) reads what applications need from uploaded videos with FFmpeg's programs: `ffprobe` for the duration, display size, codecs, frame rate and bit rate, and `ffmpeg` for a poster frame, a clean copy without the metadata a phone writes (its location among it), a web MP4 transcode and a storyboard sprite. On top of that: an `upload.Inspector` that refuses unreadable or over-limit videos, an `upload.Processor` that strips metadata, and a records plugin that stores the metadata and a poster with every video file. It is its own module because it runs external programs; the core never does, and this module imports nothing but the standard library, the library's own packages and sqlb, which records already requires (`deps_test.go` enforces it).

```sh
go get github.com/ubgo/filesystem/contrib/video
```

Needs `ffprobe` and `ffmpeg` on `PATH` (or configured paths). The guide, with output from a run: [video](../../docs/plugins/video.md). The policy of [`examples/media/video`](../../examples/media/video/main.go):

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

## API

| Name | What it is |
|---|---|
| `Tool` | runs the programs; the zero value uses the defaults. Fields: `FFprobe`, `FFmpeg`, `Timeout`, `MaxInput`, `MaxPoster`, `Formats`, `TempDir` |
| `Tool.Probe(ctx, r) (Info, error)` | `Info{Format, Duration, Width, Height, Rotation, VideoCodec, AudioCodec, FrameRate, BitRate}`; the size is the display size, rotation applied; a file without a video stream is `CodeRejected` |
| `Tool.Poster(ctx, r, at) ([]byte, error)` | the frame at `at` as a JPEG, the first frame when `at` is past the end |
| `Tool.Inspect(Limits) upload.Inspector` | refuses a stored video that is unreadable or over `Limits{MaxDuration, MaxWidth, MaxHeight}`, the length measured from the packets as well as the container's claim; other types pass |
| `Tool.Sanitize() upload.Processor` | remuxes MP4, QuickTime, WebM and Matroska without re-encoding, dropping tags, chapters and every stream but video and audio; other types pass |
| `Tool.Transcode(ctx, r, Transcode) (io.ReadCloser, error)`, `Tool.TranscodeProcessor(Transcode) upload.Processor` | a web MP4 (H.264, AAC, `+faststart`) within `MaxWidth`×`MaxHeight`, bounded by `MaxDuration` (`-t`), `MaxFrameRate`, `MaxOutput` (`-fs`) and `Timeout`; `Presets`, `MP4ContentType` |
| `Transcode.Profile *Profile`, `Profile{VideoEncoder, VideoArgs, PixelFormat, AudioEncoder, AudioArgs, Muxer, MuxerArgs, ContentType, Extension}`, `H264Profile(crf, preset, audioBitRate)`, `Transcode.ContentType()`, `Extension()` | another output than H.264 and AAC in MP4: VP9 or AV1 in WebM, HEVC, a hardware encoder. `CRF`, `Preset` and `AudioBitRate` must be zero with a profile. Names are checked, the type must be `video/*`, and args that would move a safety bound (`-t`, `-vf`, `-map`, `-f`, `-i` and the like) are `invalid_argument`. One file only: for HLS use `Tool.HLS` |
| `Tool.HLS(ctx, r, HLS, HLSWriter) error`, `HLS{Renditions, SegmentDuration, MaxDuration, MaxFrameRate, MaxOutput, Timeout, Preset, AudioBitRate}`, `Rendition{Name, MaxWidth, MaxHeight, VideoBitRate}`, `DefaultRenditions`, `HLSWriter func(ctx, name, contentType string, body io.Reader) error` | opt-in HLS ladder: H.264 and AAC renditions in MPEG-TS segments plus `HLSMaster` (`master.m3u8`), from one FFmpeg run into a work directory that is removed afterwards. A rung larger than the video is dropped except the smallest such rung, so nothing is upscaled. Transcode's bounds hold (input whitelists, `-t`, frame rate and count, first streams only, no metadata); `MaxOutput` bounds all files together and FFmpeg is stopped as the directory grows past it (`CodeTooLarge`); only the file names the ladder asked for are handed on (segments, then playlists, then the master last). `HLSPlaylistContentType`, `HLSSegmentContentType`, `MaxRenditions` (8), `DefaultHLSSegment` (6 s), `DefaultHLSMaxOutput` (4 GiB) |
| `HLS.Profile *HLSProfile{VideoEncoder, VideoArgs, PixelFormat, AudioEncoder, AudioArgs}`, `HLS.SegmentType` (`HLSSegmentMPEGTS`, `HLSSegmentFMP4`), `HLS.Encryption *HLSEncryption{Key, KeyURI, IV}`, `HLSKeyBytes` | opt-in, all nil or `""` by default (H.264, AAC, MPEG-TS, clear). A profile replaces the encoders; its names are checked (`ffmpeg.CheckNames`), `Preset` and `AudioBitRate` must then be zero, and args that move a bound or set the HLS muxer's own options (`-hls_*`, `-master_pl_name`, `-var_stream_map`, network options) are `CodeInvalidArgument`. fMP4 adds `<rendition>_init.mp4` (`HLSInitContentType`) and `.m4s` segments (`HLSFMP4SegmentContentType`), for HEVC or AV1 ladders and CMAF. Encryption is AES-128 on MPEG-TS (fMP4 is `CodeUnsupported`, as FFmpeg cannot encrypt it): the key is written to a separate work directory for FFmpeg and never reaches the writer, the playlists name `KeyURI`, and `HLSEncryption` prints and logs without its key |
| `Tool.Storyboard(ctx, r, Storyboard) (Sprite, error)`, `Sprite.WebVTT(url)` | a JPEG grid of frames for scrubbing previews and its WebVTT thumbnails track; at most `MaxTiles` tiles of at most `MaxTileSize` pixels |
| `ffmpeg.Runner` (package `contrib/video/ffmpeg`) | the bounded runner under this package and `contrib/audio`: `WorkFile`, `OutputFile`, `Run`, `Stream`, `Measure` (length from packet timestamps), `CheckLength`, `InputOptions`, `OutputLimit`, `CloseAtEOF`, `FromSeconds`/`ParseSeconds` (clamped, never negative), `MaxFileRunOutput`; refuses to run without an allowed demuxer list |
| `NewPlugin(Tool, ...PluginOption) *Plugin`, `WithPoster(at)`, `WithStrict()` | the records plugin (`PluginName` `video`) |
| `WithHLS(HLS)` | the plugin also writes an HLS ladder for every video into `<video key>.hls-<random>/` (`HLSInfix`), records the master's key in `hls_key` (and `hls_disk` when `WithLocation` chose another disk), and treats it like a poster: deleted on purge and replacement after the commit, moved with the file, kept from the orphan sweep while a row names it, deleted at once when the encode fails and on rollback. Off by default; the encode runs in the upload's transaction, so long videos belong in a job (`EncodeHLS`) |
| `Plugin.EncodeHLS(ctx, s, scope, fileID, HLS) (records.File, error)`, `Plugin.NewHLSLadder(f) (HLSLadder, error)`, `HLSLadder{Disk, Dir, SourceSHA256}.Writer(s)`, `Plugin.AttachHLS(ctx, s, scope, fileID, HLSLadder) (records.File, error)` | a ladder made in a job, recorded so it follows every `WithHLS` rule. `AttachHLS` locks the row, accepts only a ladder where `NewHLSLadder` could have put it (the Location's disk, one fresh directory under its prefix; else `CodeInvalidArgument`, so a purge can never delete more than the ladder), needs its master (`CodeNotFound`), a live released video, and the file's SHA-256 unchanged since the source was read (`CodeConflict`); it deletes the previous ladder after the commit, and a rollback deletes the new one. `EncodeHLS` deletes what it wrote when the encode or the attach fails |
| `WithLocation(Location)`, `Location func(f records.File, name string) (disk, prefix string, err error)`, `DerivedPoster`, `DerivedHLS` | where posters and ladders go: the disk (`""` for the video's own) and a key prefix; the plugin appends `<random>.jpg` (a poster) or `<random>/` and the ladder's names (HLS), so every generation writes new keys. An error fails the upload with nothing written; nil keeps the default |
| `Of(records.File) (Meta, bool)`, `PosterKey(records.File) (string, bool)`, `PosterLocation(records.File) (disk, key string, ok bool)`, `HLSLocation(records.File) (disk, key string, ok bool)` | read what the plugin recorded; `PosterLocation` and `HLSLocation` also say which disk the poster or ladder is on |
| `MetaDuration`, `MetaWidth`, `MetaHeight`, `MetaVideoCodec`, `MetaAudioCodec`, `MetaPosterKey`, `MetaPosterDisk`, `MetaHLSKey`, `MetaHLSDisk` | the metadata keys: `duration_ms`, `width`, `height`, `video_codec`, `audio_codec`, `poster_key`, `poster_disk` when `WithLocation` put the poster on another disk, and `hls_key` and `hls_disk` likewise for a ladder |
| `PosterInfix`, `PosterSuffix`, `PosterContentType` | by default posters are stored as `<video key>.poster-<random>.jpg`, `image/jpeg`, on the video's disk |
| `records.ObjectOwner`, `records.FileDiskMover`, `records.DiskRenamer` (on `*Plugin`) | `OwnedObjects` claims every poster and every file of a ladder (by the master in its directory) a file row names, live or trashed, so `Store.SweepOrphans` never deletes one; one whose file was purged is swept. `MoveFileDisk` copies the posters on the file's own disk to the new disk under the same key when `Store.MoveFile` moves the file (size checked, old object deleted after the commit, copies deleted on rollback); one `WithLocation` put on a disk of its own stays there. A poster missing from the disk fails the move. `RenameDisk` rewrites the recorded disk of every poster on the renamed disk inside `Store.RenameDisk`'s transaction, so the `Location` readers return the new name |
| `IsVideo(contentType) bool` | whether a type is `video/*` |
| `ErrNoProgram` | in the chain of the `CodeUnavailable` error of a run whose program is missing |
| `ProgramError{Program, Detail}` | the cause of a `CodeRejected` run: its text is safe for clients, `Detail` is the program's output for logs |
| `DefaultFFprobe`, `DefaultFFmpeg`, `DefaultTimeout`, `DefaultMaxInput`, `DefaultMaxPoster`, `DefaultFormats` | the defaults |

## Limits

- Every call copies the video to a work file, because MP4 files keep their index at the end and FFmpeg needs to seek; budget disk space in `TempDir` for the largest concurrent uploads.
- Tests skip only when `ffmpeg` or `ffprobe` is not installed, and say so.

## Shared metadata keys

`width` and `height` are the dimensions plugin's keys too, and `duration_ms` and `audio_codec` are contrib/audio's, so a layout reads one pair for images and videos alike. The plugin therefore sets and clears its keys only on videos: an image, an audio file or an application's own file keeps them, whichever order the plugins are installed in. When a video's content is replaced by another type, its keys are dropped, except a shared key when the new content is that plugin's type (`width` and `height` for an image, `duration_ms` and `audio_codec` for audio), which the other plugin sets; without that plugin installed, such a key keeps the video's value.

## Quarantined files

A [quarantined](../../docs/records/processing-status.md#quarantine-until-scanned) file is not given to ffprobe and gets no poster when it is recorded or replaced; it is described, with its poster, when its scan passes (`BeforeRelease`). `videoplatform`'s `Publish` refuses a quarantined file (`records.ErrQuarantined`) before calling the platform.
