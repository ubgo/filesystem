package scan_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/scan"
)

// TestHashScannerFailsWhenTheFingerprintDoes proves a fingerprint that
// cannot be computed is the scan's error, never a clean verdict, and the
// matcher is not asked: the scanner fails closed like a virus scanner
// that cannot be reached.
func TestHashScannerFailsWhenTheFingerprintDoes(t *testing.T) {
	broken := errors.New("decoder failed")
	asked := false
	s := scan.HashScanner(
		func(context.Context, io.Reader) (scan.Digest, error) { return scan.Digest{}, broken },
		scan.HashMatcherFunc(func(context.Context, scan.Digest) (scan.Verdict, error) {
			asked = true
			return scan.Verdict{Clean: true}, nil
		}),
	)
	v, err := s.Scan(t.Context(), strings.NewReader("x"))
	if !errors.Is(err, broken) || v.Clean || asked {
		t.Fatalf("scan = %+v, %v (matcher asked %t); want the fingerprint error and no verdict", v, err, asked)
	}
}
