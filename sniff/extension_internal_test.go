package sniff

import "testing"

// TestExtensionRoundTrip proves every conventional extension maps back to
// a type whose conventional extension it is, so the two tables agree.
func TestExtensionRoundTrip(t *testing.T) {
	for typ, ext := range extensions {
		if Extension(TypeByExtension("f"+ext)) != ext {
			t.Errorf("%s (%s) does not round-trip", ext, typ)
		}
	}
}
