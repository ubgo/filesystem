package scan_test

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/scan"
)

// EnvClamdAddr is the TCP address of a real clamd for TestLiveClamd
// (task test:clamav starts one in Docker).
const EnvClamdAddr = "FILESYSTEM_CLAMD_ADDR"

// clamdStartup is how long a fresh clamd may take to load its signatures.
const clamdStartup = 5 * time.Minute

// TestLiveClamd checks the client against a real clamd: the in-process
// fake only follows the protocol as documented.
func TestLiveClamd(t *testing.T) {
	addr := os.Getenv(EnvClamdAddr)
	if addr == "" {
		t.Skip(EnvClamdAddr + " is not set")
	}
	ctx := t.Context()
	c := scan.Clamd{Network: "tcp", Address: addr}
	deadline := time.Now().Add(clamdStartup)
	for err := c.Ping(ctx); err != nil; err = c.Ping(ctx) {
		if time.Now().After(deadline) {
			t.Fatalf("clamd did not answer within %v: %v", clamdStartup, err)
		}
		time.Sleep(2 * time.Second)
	}
	if v, err := c.Scan(ctx, strings.NewReader(strings.Repeat("clean ", 100000))); err != nil || !v.Clean {
		t.Fatalf("clean = %+v, %v", v, err)
	}
	v, err := c.Scan(ctx, strings.NewReader(eicar))
	if err != nil || v.Clean || !strings.Contains(strings.ToLower(v.Signature), "eicar") {
		t.Fatalf("EICAR = %+v, %v", v, err)
	}
	t.Logf("EICAR detected as %s", v.Signature)

	// clamd refuses a stream over its StreamMaxLength (its default, 100
	// MiB, in the clamd of compose.yml that task test:clamav starts) by closing early with a reply.
	_, err = c.Scan(ctx, io.LimitReader(zeros{}, overStreamMax))
	if filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Fatalf("a stream over StreamMaxLength = %v", err)
	}
}

// overStreamMax is past the StreamMaxLength of the clamd task test:clamav starts.
const overStreamMax = 101 << 20

// zeros is an endless stream of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }
