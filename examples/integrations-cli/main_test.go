package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestSession runs the whole session against a freshly built fs and pins
// the lines that do not change between runs (timestamps, ETags, ports and
// signatures do): the digests, listings, the recursive copy's report, the
// refusals with their codes and the exit codes.
func TestSession(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		// put streams and hashes what was stored; stdin works too.
		"sha256:       522a52306ab093ef4b59cbb0c41a123af96bcb7837d6e68b7d26e875d8a2bf72",
		"$ fs put 'file:srv#notes/hello.txt'\nkey:          notes/hello.txt\nsize:         17",
		// ls folds sub-directories unless -r.
		"$ fs ls file:srv\n  -  -    notes/\n  -  -  reports/\n",
		"$ fs cat 'file:srv#notes/hello.txt'\nhello from stdin\n",
		// a move inside one DSN is a rename: no digest is printed.
		"key:          notes/hi.txt\nsize:         17\n",
		`"key": "notes/hi.txt",`,
		// recursive copies list first, and a dry run changes nothing.
		"would copy reports/2026/q4.txt -> reports/2026/q4.txt\nwould copy reports/q3.txt -> reports/q3.txt\nwould copy reports/summary.txt -> reports/summary.txt\n$ fs cp -r 'file:srv#reports' 'file:bak#reports'\nreports/2026/q4.txt -> reports/2026/q4.txt\n",
		"reports/2026/q4.txt -> archive/2026/q4.txt\n",
		// a failed move reports every object and exits 1.
		`is a file, so it cannot be created` + "\nfs: 1 of 1 objects failed\n(exit 1)\n",
		// with -json the report is the whole output, codes included.
		`"failed": [`, `"code": "invalid_argument"`,
		// S3 through the fake: a cross-backend copy keeps the digest.
		"  17  ", "reports/q3.txt\n$ fs -json url -put",
		`"method": "PUT",`, "X-Amz-Expires=60",
		"\nok\n",
		// local signing needs the secret, then signs with the app's scheme.
		"unsupported: driver *local.Driver has no presign capability",
		"https://app.example.com/files/media/notes/hi.txt?exp=",
		// errors keep their codes; a usage error exits 2.
		`"code":"not_found"}` + "\n(exit 1)",
		`invalid_key: key contains a ".." segment`,
		"read_only: disk cli is read-only",
		`unknown DSN parameter "bucket" for scheme "file"`,
		"usage: fs cp [-r [-dry-run]] SOURCE TARGET\n(exit 2)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript lacks %q", want)
		}
	}
	// rm -r prints nothing and exits 0: it is the last command.
	if !strings.HasSuffix(got, "$ fs rm -r 'file:srv#notes'\n") {
		t.Errorf("rm -r printed something or failed:\n%s", got[strings.LastIndex(got, "$ fs rm"):])
	}
	if t.Failed() {
		t.Logf("transcript:\n%s", got)
	}
}

func TestQuoteArgs(t *testing.T) {
	got := quoteArgs([]string{"ls", "-r", "file:srv", "file:srv#a", "s3://b?x=1&y=2"})
	want := "ls -r file:srv 'file:srv#a' 's3://b?x=1&y=2'"
	if got != want {
		t.Errorf("quoteArgs = %q, want %q", got, want)
	}
}
