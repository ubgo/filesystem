package main

import (
	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"

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
		"documents.zip  stored users/42/imported/contracts/acme.txt (6 bytes)",
		"bomb.zip       refused: filesystem: rejected: archive: limit exceeded",
		`slip.zip       refused: filesystem: rejected: archive: unsafe entry: name "../../etc/cron.d/job"`,
		`invoice.zip    refused: filesystem: rejected: archive entry "invoice.pdf.exe" is a program`,
		"preview        contracts/acme.txt (6 bytes)", "preview        notes.md (7 bytes)",
		"on the disk: [users/42/imported/contracts/acme.txt users/42/imported/notes.md]",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestPreviewRefusesBomb pins that the preview refuses what Inspect
// refuses and leaves nothing stored.
func TestPreviewRefusesBomb(t *testing.T) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		t.Fatal(err)
	}
	bomb := zipOf([2]string{"zeros.txt", string(make([]byte, 64<<20))})
	if _, _, err := preview(t.Context(), disk, bytes.NewReader(bomb), "bomb.zip"); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("preview of a bomb = %v, want rejected", err)
	}
	for e, err := range disk.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		t.Fatalf("left on the disk: %v %v", e.Key, err)
	}
}
