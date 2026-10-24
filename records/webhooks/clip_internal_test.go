package webhooks

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestClipKeepsRunesWhole: an error message of multi-byte runes is cut to
// at most maxErrorBytes and stays valid UTF-8.
func TestClipKeepsRunesWhole(t *testing.T) {
	for _, s := range []string{strings.Repeat("é", 200), "x" + strings.Repeat("日", 100), strings.Repeat("a", 300)} {
		got := clip(s)
		if len(got) > maxErrorBytes || !utf8.ValidString(got) || !strings.HasPrefix(s, got) || len(got) < maxErrorBytes-utf8.UTFMax {
			t.Errorf("clip(%.10q…) = %d bytes, valid %v", s, len(got), utf8.ValidString(got))
		}
	}
	if clip("short") != "short" {
		t.Error("clip changed a short string")
	}
}
