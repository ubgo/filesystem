package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun runs every step and checks the lines the guide quotes.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`success 201: {"files":[{"key":"uploads/<random>.png","filename":"photo.png","content_type":"image/png","size":300,"sha256":"<hex>","path":"photo.png"}]}`,
		`failure 415: {"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}`,
		`chunk 0: 200 {"received":1,"total":3}`,
		`chunk 2: 200 {"received":2,"total":3}`,
		`chunk 2: 200 {"received":2,"total":3}`,
		`chunk 1: 201 {"files":[{"key":"uploads/<random>.png","filename":"photo.png","content_type":"image/png","size":300,"sha256":"<hex>","path":"photo.png"}]}`,
		"progress: 3 calls, last 196624 of 196624 bytes",
		"folder upload: 201",
		"users/u1/drive/Trip/day1/beach (1).png",
		"users/u1/drive/Trip/day1/beach.png",
		"users/u1/drive/Trip/day2/hike.png",
		"users/u1/drive/Trip/etc/cron.png",
		`attempt 1: 201 replayed=""`,
		`attempt 2: 200 replayed="true"`,
		`attempt 3: 422 replayed=""`,
		"objects stored: 1",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
