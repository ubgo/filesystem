package ffmpeg

import (
	"errors"
	"strings"
	"testing"
)

// TestPacketSpan_LinesSplitAcrossWrites pins that a packet line arriving
// in pieces (a pipe read can end anywhere) is held and folded once its
// newline comes, and that the final unterminated line is folded by flush.
func TestPacketSpan_LinesSplitAcrossWrites(t *testing.T) {
	var p packetSpan
	for _, chunk := range []string{"pts_time=1.5|dura", "tion_time=0.5\npts_time=0.", "25|duration_time=0.25"} {
		if n, err := p.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	if !p.seen || p.start != 1.5 || p.end != 2 {
		t.Fatalf("after two writes: %+v, want only the first line folded", p)
	}
	if err := p.flush(); err != nil {
		t.Fatal(err)
	}
	if p.start != 0.25 || p.end != 2 || len(p.line) != 0 {
		t.Fatalf("after flush: %+v", p)
	}
}

// TestPacketSpan_OverlongLineEndingInALaterWrite pins that a line held
// under the limit and completed past it by a later write is refused when
// its newline arrives, not folded.
func TestPacketSpan_OverlongLineEndingInALaterWrite(t *testing.T) {
	var p packetSpan
	head := "pts_time=1|" + strings.Repeat("x", maxPacketLine-20)
	if _, err := p.Write([]byte(head)); err != nil {
		t.Fatalf("a held line under the limit: %v", err)
	}
	if n, err := p.Write([]byte(strings.Repeat("y", 40) + "\npts_time=2\n")); n != 0 || !errors.Is(err, errPacketLine) {
		t.Fatalf("completing an overlong line = %d, %v", n, err)
	}
	if p.seen {
		t.Errorf("the overlong line was folded: %+v", p)
	}
}
