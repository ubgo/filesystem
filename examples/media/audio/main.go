// Command audio uploads a voice note the way a chat app would with
// contrib/audio: the policy refuses audio ffprobe cannot read or longer
// than a limit (Inspect) and transcodes what it accepts to mono Opus
// (TranscodeProcessor), and the records plugin stores the duration,
// codec, sample rate and a waveform of peaks beside the file. It then
// transcodes the stored note to AAC for players that need it. The note is
// a tone generated in code, so the program needs no input file.
//
//	go run ./audio      (from examples/media; needs ffmpeg and ffprobe)
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"strings"
	"time"

	"github.com/ubgo/sqlb"
	_ "modernc.org/sqlite" // the records tables live in in-memory SQLite

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/audio"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

// Decisions of this example, not of the library.
const (
	diskMedia = "media"
	scope     = "demo"
	// maxNote is the longest voice note the policy accepts.
	maxNote = 5 * time.Minute
	// waveformPoints is how many bars the player draws.
	waveformPoints = 32
	// sampleRate and toneHz shape the generated note.
	sampleRate = 16000
	toneHz     = 440
)

// tool runs FFmpeg with its defaults: programs on PATH, five minutes per
// run, at most 500 MiB copied to a work file.
var tool = audio.Tool{}

// policy accepts audio up to 25 MiB, refuses notes longer than maxNote
// (by the header and by the packets, which a client cannot patch) and
// stores every note as mono Opus, what WhatsApp and Telegram send.
// ds:def id=voicepolicy-k5w9c3nd
var policy = upload.Policy{
	MaxSize: 25 << 20,
	Allowed: []string{"audio/*"},
	Key:     upload.RandomKey("notes"),
	Process: tool.TranscodeProcessor(audio.Transcode{Format: audio.Opus, Mono: true}),
	Inspect: tool.Inspect(audio.Limits{MaxDuration: maxNote}),
}

// newStore opens the records store with the audio plugin installed,
// storing a waveform of waveformPoints peaks with each audio file.
// ds:def id=newstore-r8m3x6tj
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

// describe prints what the plugin recorded for f.
// ds:def id=describe-q2t7v4hm
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

// toAAC transcodes a stored note to AAC in MP4 (.m4a) for older Safari
// and iOS players, reading it from the disk. Run it from a job for long
// recordings: it takes about as long as the audio plays at most.
// ds:def id=toaac-w4h9n2cp
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

// run uploads the generated note and prints what was recorded.
func run(ctx context.Context, w io.Writer) error {
	s, err := newStore(ctx)
	if err != nil {
		return err
	}
	f, err := s.Upload(ctx, scope, diskMedia, bytes.NewReader(tone(2*time.Second)), "note.wav", policy)
	if err != nil {
		return err
	}
	if err := describe(w, f); err != nil {
		return err
	}
	info, err := toAAC(ctx, s, f)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "aac copy %s in %s\n", info.Codec, info.Format)
	_, err = s.Upload(ctx, scope, diskMedia, strings.NewReader("RIFF\x00\x00\x00\x00WAVEnot really audio"), "broken.wav", policy)
	fmt.Fprintf(w, "broken.wav: %s\n", filesystem.CodeOf(err))
	return nil
}

// tone is d of a 440 Hz sine as a 16-bit mono WAV file.
func tone(d time.Duration) []byte {
	n := int(d.Seconds() * sampleRate)
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+2*n))
	b.WriteString("WAVEfmt ")
	// PCM format chunk: size 16, format 1 (PCM), one channel, the sample
	// rate, the byte rate, two bytes per frame, 16 bits per sample.
	b.Write(binary.LittleEndian.AppendUint32(nil, 16))
	b.Write(binary.LittleEndian.AppendUint16(nil, 1))
	b.Write(binary.LittleEndian.AppendUint16(nil, 1))
	b.Write(binary.LittleEndian.AppendUint32(nil, sampleRate))
	b.Write(binary.LittleEndian.AppendUint32(nil, 2*sampleRate))
	b.Write(binary.LittleEndian.AppendUint16(nil, 2))
	b.Write(binary.LittleEndian.AppendUint16(nil, 16))
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(2*n))
	for i := range n {
		v := int16(math.Sin(2*math.Pi*toneHz*float64(i)/sampleRate) * 0.8 * math.MaxInt16)
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	return b.Bytes()
}

// maxOf is the largest value of v, 0 for none.
func maxOf(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		m = max(m, x)
	}
	return m
}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatal(err)
	}
}
