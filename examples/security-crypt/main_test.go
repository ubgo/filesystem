package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"acme/finance/q3.txt: 260000 bytes written, 261088 bytes stored",
		"backend holds the plaintext: false",
		`ranged read at 200000: "ue: confidential. Q3 reven"`,
		"rotated 1 acme object(s); readable without the old key: true",
		`globex after its key was deleted: filesystem: read vault:globex/hr/salaries.csv: permission: crypt: no master key "tenant-globex-v0"`,
		"printing the KMS: crypt.MemoryKMS{1 keys, redacted}",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
