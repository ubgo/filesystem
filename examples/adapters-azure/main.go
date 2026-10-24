// Command adapters-azure walks through the Azure Blob Storage driver:
// opening a disk from a DSN (the connection string stays in the
// environment) and from a Config, SAS signed URLs, a server-side multipart
// upload, a conditional write, and the capabilities Azure does not have.
//
// There is no in-process Azure fake, so it needs a real account or
// Azurite, the emulator (task azure:up starts one on 127.0.0.1:10000):
//
//	export AZURE_STORAGE_CONNECTION_STRING='DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=…;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;'
//	go run ./adapters-azure            (from the examples directory)
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
)

// containerName is the container this program uses; it is created when
// missing.
const containerName = "media"

// linkLifetime is how long the SAS URLs in this program stay valid.
const linkLifetime = 15 * time.Minute

// headerBlobType is the header a client must send with a signed PUT so
// Azure creates a block blob; SignedRequest.Header carries it.
const headerBlobType = "X-Ms-Blob-Type"

func main() {
	// The offline steps run against in-process stand-ins, so they need no
	// Azure account; the rest needs Azurite or a real account.
	if err := offline(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if os.Getenv(azure.DefaultConnectionEnv) == "" {
		fmt.Fprintf(os.Stderr, "set %s to an Azure or Azurite connection string\n", azure.DefaultConnectionEnv)
		os.Exit(2)
	}
	if err := run(context.Background(), os.Stdout, containerName); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is main without the process: it opens container through the DSN,
// creating it when missing, and prints what each step saw to out.
func run(ctx context.Context, out io.Writer, container string) error {
	disk, err := openWithDSN(ctx, container)
	if err != nil {
		return err
	}
	if err := ensureContainer(ctx, disk, container); err != nil {
		return err
	}
	fmt.Fprintf(out, "capabilities: %s\n", disk.Capabilities())
	steps := []func(context.Context, io.Writer, *filesystem.Disk) error{
		writeOnce, sasLinks, serverMultipart, refusedOptions, checksums,
	}
	for _, step := range steps {
		if err := step(ctx, out, disk); err != nil {
			return err
		}
	}
	return nil
}

// openWithDSN opens a container through a Mux. The DSN names only the
// container; the account name and key come from the connection string in
// AZURE_STORAGE_CONNECTION_STRING (or the variable named by connection_env).
// ds:def id=openwithdsn-bq3tj2xf
func openWithDSN(ctx context.Context, container string) (*filesystem.Disk, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(azure.Scheme, azure.Opener); err != nil {
		return nil, err
	}
	return mux.Open(ctx, "media", azure.Scheme+"://"+container)
}

// openWithConfig builds the driver from an account client the application
// made itself. SharedKey is what enables SAS signing; without it the disk
// works but SignedURL is CodeUnsupported.
// ds:def id=openwithconfig-rqy7ht78
func openWithConfig(accountURL, accountName, accountKey, container string) (*filesystem.Disk, error) {
	cred, err := azblob.NewSharedKeyCredential(accountName, accountKey)
	if err != nil {
		return nil, err
	}
	client, err := azblob.NewClientWithSharedKeyCredential(accountURL, cred, nil)
	if err != nil {
		return nil, err
	}
	drv, err := azure.New(azure.Config{Client: client, Container: container, SharedKey: cred})
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk("media", drv)
}

// ensureContainer creates the container through the native client, the
// filesystem.As escape hatch. A container that already exists is fine.
// ds:def id=ensurecontainer-4darszyg
func ensureContainer(ctx context.Context, disk *filesystem.Disk, container string) error {
	client, ok := filesystem.As[*azblob.Client](disk)
	if !ok {
		return fmt.Errorf("disk %s is not an Azure disk", disk.Name())
	}
	_, err := client.CreateContainer(ctx, container, nil)
	if bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
		return nil
	}
	return err
}

// writeOnce writes a blob only when it does not exist yet (If-None-Match
// on the commit), so the second attempt is CodeExists.
// ds:def id=writeonce-xwgv3s3k
func writeOnce(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	key := "receipts/" + strings.ToLower(rand.Text()) + ".txt"
	opts := filesystem.WriteOptions{
		IfNotExists: true,
		Metadata:    map[string]string{"order": "1042"},
		Headers:     filesystem.HTTPHeaders{CacheControl: "private, max-age=0"},
	}
	if _, err := disk.WriteBytes(ctx, key, []byte("paid"), opts); err != nil {
		return err
	}
	_, err := disk.WriteBytes(ctx, key, []byte("paid twice"), opts)
	fmt.Fprintf(out, "second conditional write: %s\n", filesystem.CodeOf(err))
	e, err := disk.Stat(ctx, key)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stat: %d bytes, order=%s, cache-control=%q\n", e.Size, e.Metadata["order"], e.Headers.CacheControl)
	return nil
}

// sasLinks issues a SAS upload URL, uploads through it with the headers it
// requires, then downloads through a SAS GET URL.
// ds:def id=saslinks-bfgr6paa
func sasLinks(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	put, err := disk.SignedURL(ctx, "inbox/scan.txt", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: linkLifetime, ContentType: "text/plain"})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signed PUT requires %s: %s\n", headerBlobType, put.Header.Get(headerBlobType))
	if _, err := send(ctx, http.MethodPut, put.URL, strings.NewReader("scanned"), put.Header); err != nil {
		return err
	}
	get, err := disk.SignedURL(ctx, "inbox/scan.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	body, err := send(ctx, http.MethodGet, get.URL, nil, get.Header)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signed GET: %q\n", body)
	return nil
}

// serverMultipart uploads two parts through the server as staged blocks
// and commits them. Parts may be any size; Azure keeps uncommitted blocks
// for seven days.
// ds:def id=servermultipart-ayhqr3w8
func serverMultipart(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	const key = "exports/big.csv"
	id, err := disk.CreateMultipart(ctx, key, filesystem.WriteOptions{ContentType: "text/csv"})
	if err != nil {
		return err
	}
	var parts []filesystem.Part
	for i, chunk := range []string{"id,total\n", "1,42\n"} {
		p, err := disk.UploadPart(ctx, key, id, i+1, strings.NewReader(chunk), int64(len(chunk)))
		if err != nil {
			return err
		}
		parts = append(parts, p)
	}
	e, err := disk.CompleteMultipart(ctx, key, id, parts)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "multipart: %s is %d bytes, %s\n", e.Key, e.Size, e.ContentType)
	return nil
}

// refusedOptions shows what Azure cannot do per object: public visibility
// (a container setting) and presigned parts for browser uploaders.
// ds:def id=refusedoptions-wwr4ncm8
func refusedOptions(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	_, err := disk.WriteBytes(ctx, "public.txt", []byte("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic})
	fmt.Fprintf(out, "public write: %s\n", filesystem.CodeOf(err))
	_, err = disk.PresignPart(ctx, "k", "id", 1, linkLifetime)
	fmt.Fprintf(out, "presigned part: %s\n", filesystem.CodeOf(err))
	return nil
}

// checksums writes one blob with the SHA-256 its body has and one with a
// wrong digest. The Disk hashes every body and refuses the mismatch before
// the block list is committed, so nothing appears at the second key. Azure
// does not verify SHA-256 itself; it checks per-block CRC64 when
// Config.ValidateCRC64 is on.
// ds:def id=checksums-bu9zpz2k
func checksums(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	body := []byte("invoice 1042, total 42.00")
	sum := sha256.Sum256(body)
	if _, err := disk.WriteBytes(ctx, "invoices/1042.txt", body, filesystem.WriteOptions{SHA256: hex.EncodeToString(sum[:])}); err != nil {
		return err
	}
	wrong := sha256.Sum256([]byte("tampered"))
	_, err := disk.WriteBytes(ctx, "invoices/1043.txt", body, filesystem.WriteOptions{SHA256: hex.EncodeToString(wrong[:])})
	stored, existsErr := disk.Exists(ctx, "invoices/1043.txt")
	if existsErr != nil {
		return existsErr
	}
	fmt.Fprintf(out, "checksums: matching digest stored, wrong digest %s, stored=%t\n", filesystem.CodeOf(err), stored)
	return nil
}

// send performs one HTTP request with the given headers and returns the
// body, failing on a non-2xx status.
func send(ctx context.Context, method, rawURL string, body io.Reader, h http.Header) (string, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return "", err
	}
	for k, v := range h {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err == nil && resp.StatusCode/100 != 2 {
		path, _, _ := strings.Cut(rawURL, "?") // the query holds the signature
		err = fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return string(b), err
}
