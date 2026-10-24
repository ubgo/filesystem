package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/adapters/azure"
)

// envTestConnection holds a connection string of an Azure account or of
// Azurite (task test:azure sets it for the adapter). There is no
// in-process Azure fake, so without it these tests skip.
const envTestConnection = "FILESYSTEM_AZURE_TEST_CONNECTION"

// connection returns the test connection string, or skips the test.
func connection(t *testing.T) string {
	t.Helper()
	cs := os.Getenv(envTestConnection)
	if cs == "" {
		t.Skipf("set %s to run against Azure or Azurite", envTestConnection)
	}
	return cs
}

// TestRun walks the whole program in a fresh container and checks the
// lines the guide quotes.
func TestRun(t *testing.T) {
	t.Setenv(azure.DefaultConnectionEnv, connection(t))
	container := "c" + strings.ToLower(rand.Text())
	var out bytes.Buffer
	if err := run(t.Context(), &out, container); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		"capabilities: [copy,presign,multipart,sweep-abandoned,find-abandoned,metadata,content-type,headers,presign-ip,conditional-write,append,tags,hold,storage-class,restore,compare-and-swap]",
		"second conditional write: exists",
		`stat: 4 bytes, order=1042, cache-control="private, max-age=0"`,
		"signed PUT requires X-Ms-Blob-Type: BlockBlob",
		`signed GET: "scanned"`,
		"multipart: exports/big.csv is 14 bytes, text/csv",
		"public write: unsupported",
		"presigned part: unsupported",
		"checksums: matching digest stored, wrong digest invalid_argument, stored=false",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestOpenWithConfig builds the same disk from a Config, with the account
// values the connection string carries.
func TestOpenWithConfig(t *testing.T) {
	fields := map[string]string{}
	for _, part := range strings.Split(connection(t), ";") {
		if k, v, ok := strings.Cut(part, "="); ok {
			fields[k] = v
		}
	}
	disk, err := openWithConfig(strings.TrimRight(fields["BlobEndpoint"], "/")+"/", fields["AccountName"], fields["AccountKey"], containerName)
	if err != nil {
		t.Fatal(err)
	}
	if c := disk.Capabilities(); !c.Presign {
		t.Errorf("SharedKey did not enable Presign: %s", c)
	}
}

// TestLargeBlobs runs the block-size step, which needs no Azure account,
// and checks the blocks each configuration staged and the refusals.
func TestLargeBlobs(t *testing.T) {
	var out bytes.Buffer
	if err := largeBlobs(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`defaults: 9 MiB staged as 2 blocks (MiB: 8,1), at most 5 in flight: true`,
		`2 MiB blocks, one at a time: 5 MiB staged as 3 blocks (MiB: 2,2,1), at most 1 in flight: true`,
		`1 MiB blocks, three at once: 9 MiB staged as 9 blocks (MiB: 1,1,1,1,1,1,1,1,1), at most 3 in flight: true`,
		`512 KiB blocks: invalid_argument`,
		`4001 MiB blocks: invalid_argument`,
		`negative concurrency: invalid_argument`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestOfflineSteps runs the per-call options and front-end steps, which
// need no Azure account, and checks what the stand-in saw.
func TestOfflineSteps(t *testing.T) {
	var out bytes.Buffer
	for _, step := range []func(context.Context, io.Writer) error{perCall, frontDoor} {
		if err := step(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{
		`per call: contracts/2026/acme.pdf tier="Cool" scope="tenant-acme" tags="tenant=acme"`,
		`per call: thumbs/acme.png tier="" scope="" tags=""`,
		`per call: legal holds set on /legal/contracts/2026/acme.pdf`,
		`per call: unknown tier invalid_argument, request sent false`,
		`front door: key requests went to the configured host: true, signed URL keeps it: true`,
		`front door: signature differs from the host-label account's: true`,
		`front door: AccountName without Credential: invalid_argument`,
		`front door: AccountName "Files.Example": invalid_argument`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
