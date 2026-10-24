package audio

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"strconv"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video/ffmpeg"
)

// MaxWaveformPoints bounds Waveform's points: more than a wide screen has
// pixels is never drawn.
const MaxWaveformPoints = 4096

// waveformRate is the sample rate audio is decoded at for peaks: far
// below hearing quality, plenty for bars a few pixels wide, and it keeps
// the decoded stream to 16 KB a second.
const waveformRate = 8000

// bytesPerSample of the signed 16-bit PCM decoded for peaks.
const bytesPerSample = 2

// Waveform returns points peak levels evenly spread over r's length, each
// the loudest sample of its span as a fraction of full scale in [0, 1]:
// the bars of a voice-note or podcast player. Audio longer than
// MaxDuration, or of unknown length, is CodeRejected; points outside 1 to
// MaxWaveformPoints are CodeInvalidArgument. The decoded samples are
// folded into the peaks as FFmpeg writes them, so memory stays at the
// size of the result whatever the length.
func (t Tool) Waveform(ctx context.Context, r io.Reader, points int) ([]float64, error) {
	if points < 1 || points > MaxWaveformPoints {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "audio: waveform points must be 1 to %d", MaxWaveformPoints)
	}
	var peaks []float64
	err := t.runner().WorkFile(r, func(path string) error {
		var err error
		info, err := t.probePath(ctx, path)
		if err != nil {
			return err
		}
		peaks, err = t.peaks(ctx, path, info, points)
		return err
	})
	if err != nil {
		return nil, err
	}
	return peaks, nil
}

// peaks computes points peaks of the audio at path, which ffprobe
// described as info.
func (t Tool) peaks(ctx context.Context, path string, info Info, points int) ([]float64, error) {
	if err := t.bounded(info); err != nil {
		return nil, err
	}
	samples := int64(math.Ceil(info.Duration.Seconds() * waveformRate))
	acc := &peakWriter{peaks: make([]float64, points), perPoint: max(1, (samples+int64(points)-1)/int64(points))}
	args := append([]string{"-v", "error", "-nostdin"}, t.runner().InputOptions()...)
	args = append(args, "-i", path, "-t", ffmpeg.Seconds(t.maxDuration()), "-map", "0:a:0",
		"-ac", "1", "-ar", strconv.Itoa(waveformRate), "-f", "s16le", "-c:a", "pcm_s16le", "pipe:1")
	limit := (int64(t.maxDuration().Seconds()) + 1) * waveformRate * bytesPerSample
	if err := t.runner().Stream(ctx, t.ffmpeg(), args, acc, limit); err != nil {
		return nil, err
	}
	return acc.peaks, nil
}

// peakWriter folds signed 16-bit little-endian mono samples into peaks,
// perPoint samples to a point; samples past the last point (a container
// that understated its length) fold into the last one.
type peakWriter struct {
	peaks    []float64
	perPoint int64
	n        int64
	carry    []byte
}

// fullScale is the magnitude of the most negative 16-bit sample.
const fullScale = 32768

func (w *peakWriter) Write(p []byte) (int, error) {
	in := p
	if len(w.carry) > 0 {
		in = append(w.carry, p...)
		w.carry = nil
	}
	for len(in) >= bytesPerSample {
		v := math.Abs(float64(int16(binary.LittleEndian.Uint16(in)))) / fullScale
		i := min(w.n/w.perPoint, int64(len(w.peaks)-1))
		w.peaks[i] = max(w.peaks[i], v)
		w.n++
		in = in[bytesPerSample:]
	}
	if len(in) > 0 {
		w.carry = []byte{in[0]}
	}
	return len(p), nil
}
