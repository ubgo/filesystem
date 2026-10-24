package azure_test

import (
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
	"github.com/ubgo/filesystem/filesystemtest"
)

// EnvTestConnection holds a connection string of an Azure account or of
// Azurite (task test:azure starts one). Without it, only the tests that
// need no server run.
const EnvTestConnection = "FILESYSTEM_AZURE_TEST_CONNECTION"

// account returns a client and shared key for the test account.
func account(t *testing.T) (*azblob.Client, *azblob.SharedKeyCredential, string) {
	t.Helper()
	cs := os.Getenv(EnvTestConnection)
	if cs == "" {
		t.Skipf("set %s (task test:azure) to run against Azure or Azurite", EnvTestConnection)
	}
	t.Setenv(azure.DefaultConnectionEnv, cs)
	fields := map[string]string{}
	for _, part := range strings.Split(cs, ";") {
		if k, v, ok := strings.Cut(part, "="); ok {
			fields[k] = v
		}
	}
	cred, err := azblob.NewSharedKeyCredential(fields["AccountName"], fields["AccountKey"])
	if err != nil {
		t.Fatal(err)
	}
	client, err := azblob.NewClientWithSharedKeyCredential(strings.TrimRight(fields["BlobEndpoint"], "/")+"/", cred, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client, cred, cs
}

func newContainer(t *testing.T, client *azblob.Client) string {
	t.Helper()
	name := "c" + strings.ToLower(rand.Text())
	if _, err := client.CreateContainer(t.Context(), name, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = client.DeleteContainer(t.Context(), name, nil) })
	return name
}

func TestConformance(t *testing.T) {
	client, cred, _ := account(t)
	filesystemtest.RunWith(t, func(t *testing.T) *filesystem.Disk {
		drv, err := azure.New(azure.Config{Client: client, Container: newContainer(t, client), SharedKey: cred})
		if err != nil {
			t.Fatal(err)
		}
		d, err := filesystem.NewDisk("azure", drv)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}, filesystemtest.Options{StorageClass: string(blob.AccessTierCool)})
}

func TestNewValidation(t *testing.T) {
	if _, err := azure.New(azure.Config{Container: "c"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("no client = %v", err)
	}
	client, err := azblob.NewClientWithNoCredential("http://127.0.0.1:1/x/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := azure.New(azure.Config{Client: client}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("no container = %v", err)
	}
	drv, _ := azure.New(azure.Config{Client: client, Container: "c"})
	if c := drv.Capabilities(); c.Presign || c.PresignPart || !c.Multipart || c.SetVisibility {
		t.Errorf("capabilities without a key = %v", c)
	}
	d, _ := filesystem.NewDisk("azure", drv)
	if got, ok := filesystem.As[*azblob.Client](d); !ok || got != client {
		t.Error("As did not return the client")
	}
	// Errors from an unreachable account classify without a server.
	_, err = d.Stat(t.Context(), "k")
	if err == nil {
		t.Error("stat against nothing succeeded")
	}
	var resp *azcore.ResponseError
	if errors.As(err, &resp) {
		t.Errorf("connection failure looked like a service response: %v", err)
	}
}
