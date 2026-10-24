package upload

import "testing"

// TestGuardAddress covers the dial guard's defence against an address the
// dialer would never pass.
func TestGuardAddress(t *testing.T) {
	if err := (FetchOptions{}).guard("tcp", "not-an-address", nil); err == nil {
		t.Fatal("a malformed address passed the guard")
	}
}
