package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun checks the configuration behaviour the page describes.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"key logo.png, public, url https://cdn.example.com/tenant-a/logo.png, on disk under tenant-a: true",
		`typo: filesystem: invalid_argument: disk media: unknown DSN parameter "visiblity" for scheme "mem"`,
		`unregistered: filesystem: invalid_argument: disk media: no opener registered for scheme "s3"`,
		"readonly write: read_only",
		"disks backups,media, default media",
		"variable for media-cdn: STORAGE_MEDIA_CDN_DSN",
		"check: every disk works",
		"copied to backups:2026/report.csv (text/csv; charset=utf-8)",
		"unknown disk: not_found",
		`missing: filesystem: invalid_argument: STORAGE_BACKUPS_DSN is not set (disk "backups")`,
		"10MB = 10000000 bytes, 1.5GiB = 1610612736 bytes, 10*MiB = 10485760",
		`ParseSize("10mb"): invalid_argument`,
		`ParseSize("10 MB"): invalid_argument`,
		`ParseSize("1.5B"): invalid_argument`,
		`ParseSize("-1KB"): invalid_argument`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "topsecret") {
		t.Errorf("an error printed the DSN's credentials:\n%s", got)
	}
}
