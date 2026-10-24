package gdrive_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gdrive"
)

// TestNativeClient pins the filesystem.As escape hatch: a disk over the
// driver hands out the driver's *http.Client, which is a copy of the
// caller's Config.Client (so the driver's redirect policy never mutates the
// caller's client) or, without one, a client bounded by DefaultTimeout.
func TestNativeClient(t *testing.T) {
	const callerTimeout = 7 * time.Second
	for _, tc := range []struct {
		name        string
		client      *http.Client
		wantTimeout time.Duration
	}{
		{"default", nil, gdrive.DefaultTimeout},
		{"caller", &http.Client{Timeout: callerTimeout}, callerTimeout},
	} {
		cfg := config(newFake(t))
		cfg.Client = tc.client
		drv, err := gdrive.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		disk, err := filesystem.NewDisk(tc.name, drv)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := filesystem.As[*http.Client](disk)
		if !ok || got == nil {
			t.Fatalf("%s: As[*http.Client] = %v, %v", tc.name, got, ok)
		}
		if got.Timeout != tc.wantTimeout {
			t.Errorf("%s: timeout %v, want %v", tc.name, got.Timeout, tc.wantTimeout)
		}
		if tc.client != nil && got == tc.client {
			t.Errorf("%s: the driver uses the caller's client itself, not a copy", tc.name)
		}
	}
}
