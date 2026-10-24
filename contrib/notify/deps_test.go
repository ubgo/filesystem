package notify_test

import (
	"os"
	"strings"
	"testing"
)

// TestGuard_RequiresNothing keeps the vendor-neutral seam free of
// dependencies: the SDKs belong to the consumer modules (sqs, pubsub),
// so an application using one cloud never downloads another's.
func TestGuard_RequiresNothing(t *testing.T) {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "require") {
		t.Fatalf("contrib/notify must require no module:\n%s", b)
	}
}
