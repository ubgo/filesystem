package main

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRun drives the whole flow against the in-process fake: page,
// ticket, form post, confirm, and the oversize upload Confirm catches.
func TestRun(t *testing.T) {
	disk, stop, err := openDisk(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	var out bytes.Buffer
	if err := run(t.Context(), &out, disk); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		"ticket: POST 127.0.0.1 key=avatars/<random>.png",
		"bucket accepted the form: 200",
		"confirm: 200 image/png 67 bytes, key avatars/<random>.png",
		"oversize upload: accepted by the store (200), which does not check the policy",
		"confirm: 415 stored size 1091 is not the 67 bytes declared (object kept: false)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestTicketRefusals proves the application refuses a ticket for a file
// the policy does not accept, before anything is signed.
func TestTicketRefusals(t *testing.T) {
	disk, stop, err := openDisk(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	srv := httptest.NewServer(app(disk))
	t.Cleanup(srv.Close)
	b := browser{app: srv.URL}
	for name, c := range map[string]struct {
		file, contentType string
		size              int64
	}{
		"another type": {"a.txt", "text/plain", 10},
		"too large":    {"a.png", avatarType, avatarMaxSize + 1},
		"empty":        {"a.png", avatarType, 0},
	} {
		if _, err := b.ticket(t.Context(), c.file, c.contentType, c.size); err == nil {
			t.Errorf("%s: ticket issued", name)
		}
	}
}
