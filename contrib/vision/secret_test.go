package vision_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/contrib/vision"
)

// TestSecret_NeverPrints pins the redaction of the Secret itself (the
// google adapter's test covers a Config holding one): every fmt verb and
// both slog handlers print the placeholder, never the value, while Reveal
// still returns it and the zero Secret is empty.
func TestSecret_NeverPrints(t *testing.T) {
	const key = "key-SECRET-1"
	s := vision.NewSecret(key)
	var out bytes.Buffer
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		fmt.Fprintf(&out, verb+"\n", s)
	}
	fmt.Fprintf(&out, "%s\n%s\n", s.String(), s.GoString())
	slog.New(slog.NewTextHandler(&out, nil)).Info("m", "key", s)
	slog.New(slog.NewJSONHandler(&out, nil)).Info("m", "key", s)
	if strings.Contains(out.String(), "SECRET") {
		t.Fatalf("the secret printed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "[redacted]") {
		t.Fatalf("no redaction marker:\n%s", out.String())
	}
	if s.Reveal() != key || s.IsZero() {
		t.Fatalf("Reveal = %q, IsZero = %v; want the value and false", s.Reveal(), s.IsZero())
	}
	if !vision.NewSecret("").IsZero() || (vision.Secret{}).Reveal() != "" {
		t.Fatal("an empty Secret is not the zero Secret")
	}
}
