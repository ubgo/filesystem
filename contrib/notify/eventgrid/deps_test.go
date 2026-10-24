package eventgrid_test

import (
	"os"
	"regexp"
	"testing"
)

// TestGuard_RequiresOnlyNotify keeps the Event Grid webhook on the
// standard library: it parses the event schema itself instead of pulling
// in the Azure SDK.
func TestGuard_RequiresOnlyNotify(t *testing.T) {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`(?m)^\s*(?:require\s+)?([a-z0-9.\-]+\.[a-z]+/\S+)\s+v\S+`).FindAllStringSubmatch(string(b), -1) {
		if m[1] != "github.com/ubgo/filesystem/contrib/notify" {
			t.Fatalf("eventgrid requires %s", m[1])
		}
	}
}
