package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestEveryProviderDSNOpens opens each DSN the guide lists and checks the
// flags it declares, so a typo in a published DSN fails here first.
func TestEveryProviderDSNOpens(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != len(providers) {
		t.Fatalf("%d lines for %d providers:\n%s", len(lines), len(providers), out.String())
	}
	for _, want := range []string{
		"AWS S3                   visibility=false conditional=true",
		"AWS S3 with object ACLs  visibility=true  conditional=true",
		"Cloudflare R2            visibility=false conditional=false",
		"Backblaze B2             visibility=false conditional=false",
		"SeaweedFS                visibility=false conditional=true",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestFakeRoundTrip runs the provider-shaped DSN against the in-process
// S3 and checks the refusals the guide quotes.
func TestFakeRoundTrip(t *testing.T) {
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")
	t.Setenv(accessKeyEnv, "")
	t.Setenv(secretKeyEnv, "")
	var out bytes.Buffer
	if err := runFake(t.Context(), &out); err != nil {
		t.Fatalf("runFake: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		`round trip: read back "meow"`,
		"listed: photos/cat.txt 4 bytes",
		"if-not-exists without conditional_write: unsupported",
		"refused DSN (keys in the URL): invalid_argument",
		"refused DSN (access_key_env alone): invalid_argument",
		"refused DSN (unset key variable): invalid_argument",
		"refused DSN (path_style=yes): invalid_argument",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
