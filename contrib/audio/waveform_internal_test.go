package audio

import (
	"encoding/binary"
	"testing"
)

// TestPeakWriter_SamplesSplitAcrossWrites pins the carry: a pipe may
// split a sample's two bytes across writes, and the sample must still
// count once, whole.
func TestPeakWriter_SamplesSplitAcrossWrites(t *testing.T) {
	pcm := make([]byte, 0, 8)
	for _, v := range []int16{100, -32768, 50, 16384} {
		pcm = binary.LittleEndian.AppendUint16(pcm, uint16(v))
	}
	w := &peakWriter{peaks: make([]float64, 2), perPoint: 2}
	for _, chunk := range [][]byte{pcm[:3], pcm[3:5], pcm[5:]} {
		if n, err := w.Write(chunk); err != nil || n != len(chunk) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if w.n != 4 || w.peaks[0] != 1 || w.peaks[1] != 0.5 {
		t.Fatalf("peaks = %v after %d samples", w.peaks, w.n)
	}
	// Samples past the last point fold into it.
	loud := int16(-24576)
	_, _ = w.Write(binary.LittleEndian.AppendUint16(nil, uint16(loud)))
	if w.peaks[1] != 0.75 {
		t.Fatalf("overflow sample: %v", w.peaks)
	}
}
