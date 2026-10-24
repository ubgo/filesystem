package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun opens the Supabase-shaped DSN against the stand-in project and
// checks the lines the guide quotes, including that the first request
// went under /storage/v1/s3.
func TestRun(t *testing.T) {
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")
	t.Setenv(accessKeyEnv, "")
	t.Setenv(secretKeyEnv, "")
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		"capabilities: [copy,delete-prefix,delete-keys,presign,",
		"stored: users/42/profile.json 14 bytes application/json",
		"listed: users/42/profile.json",
		`read back: {"name":"Ada"}`,
		"exists after delete: false",
		"first request: PUT /storage/v1/s3/avatars/users/42/profile.json",
		"if-not-exists: unsupported",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
