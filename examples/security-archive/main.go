// Command security-archive accepts zip uploads safely: every upload is
// walked under limits by an upload.Policy inspector before it is reported
// stored, and a clean one is then extracted into the importing user's
// folder. A zip bomb, a zip-slip archive and an archive hiding a program
// are each refused, and nothing of them is kept.
//
//	go run ./security-archive
//
// The archives are built in memory by the program, so the attacks are
// readable below; files go to the memory driver.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/archive"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/sniff"
	"github.com/ubgo/filesystem/upload"
)

// Settings of the example: an import of a folder of documents.
const (
	diskName      = "imports"
	maxUploadSize = 50 << 20
	// importPrefix is where one user's extracted files go.
	importPrefix = "users/42/imported"
)

// limits are what one import may expand to: fewer entries and bytes than
// the defaults, because an import is a folder of documents, not a backup.
var limits = archive.Limits{MaxEntries: 1000, MaxTotal: 200 << 20}

// newPolicy accepts zip files only, and inspects each one as an archive
// before the upload succeeds.
// ds:def id=newpolicy-3jkc4cve
func newPolicy() upload.Policy {
	return upload.Policy{
		MaxSize: maxUploadSize,
		Allowed: []string{sniff.Zip},
		Inspect: archive.Inspect(limits, archive.DenyExecutables),
	}
}

// importZip stores an uploaded zip under the policy, then extracts it.
// The upload already proved the archive within the limits, and Extract
// enforces them again while it writes, so a file changed in between is
// caught too.
// ds:def id=importzip-9tm4vbxg
func importZip(ctx context.Context, disk *filesystem.Disk, body io.Reader, name string) ([]archive.Entry, error) {
	res, err := upload.Receive(ctx, disk, body, name, newPolicy())
	if err != nil {
		return nil, err
	}
	defer func() { _ = disk.Delete(context.WithoutCancel(ctx), res.Key) }()
	src, err := archive.OpenReaderAt(ctx, disk, res.Key)
	if err != nil {
		return nil, err
	}
	return archive.Extract(ctx, disk, importPrefix, src, src.Size(), archive.ExtractOptions{Limits: limits})
}

// preview stores an uploaded zip under the policy and lists what it holds
// without extracting anything, so the user can confirm or pick entries
// first. List reads only the central directory and refuses the same
// archives Inspect does (too many entries, too many bytes, unsafe names).
// The stored zip is kept for the extraction that follows; sweep the ones
// never confirmed.
// ds:def id=preview-r3w8k6tn
func preview(ctx context.Context, disk *filesystem.Disk, body io.Reader, name string) (string, []archive.Entry, error) {
	res, err := upload.Receive(ctx, disk, body, name, newPolicy())
	if err != nil {
		return "", nil, err
	}
	src, err := archive.OpenReaderAt(ctx, disk, res.Key)
	if err != nil {
		return "", nil, err
	}
	entries, err := archive.List(ctx, src, src.Size(), limits)
	if err != nil {
		_ = disk.Delete(context.WithoutCancel(ctx), res.Key)
		return "", nil, err
	}
	return res.Key, entries, nil
}

// zipOf builds a zip of name/content pairs.
func zipOf(files ...[2]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		w, err := zw.Create(f[0])
		if err != nil {
			log.Fatal(err)
		}
		if _, err := io.WriteString(w, f[1]); err != nil {
			log.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		log.Fatal(err)
	}
	return buf.Bytes()
}

func run(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return err
	}
	uploads := []struct {
		name string
		body []byte
	}{
		{"documents.zip", zipOf([2]string{"contracts/acme.txt", "signed"}, [2]string{"notes.md", "# notes"})},
		{"bomb.zip", zipOf([2]string{"zeros.txt", string(make([]byte, 64<<20))})},
		{"slip.zip", zipOf([2]string{"ok.txt", "ok"}, [2]string{"../../etc/cron.d/job", "* * * * * curl evil"})},
		{"invoice.zip", zipOf([2]string{"invoice.pdf.exe", "MZ fake program"})},
	}
	for _, u := range uploads {
		entries, err := importZip(ctx, disk, bytes.NewReader(u.body), u.name)
		if err != nil {
			fmt.Fprintf(out, "%-14s refused: %v\n", u.name, err)
			continue
		}
		for _, e := range entries {
			fmt.Fprintf(out, "%-14s stored %s (%d bytes)\n", u.name, filesystem.JoinKey(importPrefix, e.Name), e.Size)
		}
	}
	key, listed, err := preview(ctx, disk, bytes.NewReader(uploads[0].body), uploads[0].name)
	if err != nil {
		return err
	}
	for _, e := range listed {
		fmt.Fprintf(out, "preview        %s (%d bytes)\n", e.Name, e.Size)
	}
	if err := disk.Delete(ctx, key); err != nil {
		return err
	}
	var keys []string
	for e, err := range disk.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		keys = append(keys, e.Key)
	}
	fmt.Fprintf(out, "on the disk: %v\n", keys)
	return nil
}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatal(err)
	}
}
