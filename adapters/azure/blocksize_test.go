package azure_test

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
)

// stageHold is how long the counting server holds each staged block, so
// blocks staged at once overlap and the peak shows the concurrency.
const stageHold = 40 * time.Millisecond

// blockCounter wraps a scripted service and records, for every staged
// block (Put Block), its size and how many were in flight at once.
type blockCounter struct {
	*scripted
	mu       sync.Mutex
	sizes    []int64
	inflight int
	peak     int
}

func (b *blockCounter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut && r.URL.Query().Get("comp") == "block" {
		b.mu.Lock()
		b.sizes = append(b.sizes, r.ContentLength)
		b.inflight++
		b.peak = max(b.peak, b.inflight)
		b.mu.Unlock()
		time.Sleep(stageHold)
		defer func() { b.mu.Lock(); b.inflight--; b.mu.Unlock() }()
	}
	b.scripted.ServeHTTP(w, r)
}

// counted returns the staged block sizes, sorted, and the peak in flight.
func (b *blockCounter) counted() ([]int64, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sizes := slices.Clone(b.sizes)
	slices.Sort(sizes)
	return sizes, b.peak
}

// countingServer starts a blockCounter answering Get Blob Properties for
// key in container "c", and returns it with its URL.
func countingServer(t *testing.T, key string) (*blockCounter, string) {
	t.Helper()
	b := &blockCounter{scripted: &scripted{}}
	b.on("HEAD /c/"+key, reply{status: http.StatusOK, header: propsHeaders(nil)})
	srv := httptest.NewServer(b)
	t.Cleanup(srv.Close)
	return b, srv.URL
}

// countingDisk returns a disk with cfg on a countingServer.
func countingDisk(t *testing.T, key string, cfg azure.Config) (*filesystem.Disk, *blockCounter) {
	t.Helper()
	b, url := countingServer(t, key)
	client, err := azblob.NewClientWithNoCredential(url+"/", &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Client, cfg.Container = client, "c"
	drv, err := azure.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("azure", drv)
	if err != nil {
		t.Fatal(err)
	}
	return d, b
}

// mib is n mebibytes.
func mib(n int64) int64 { return n << 20 }

// TestWriteBlockSizeAndConcurrency proves Write stages blocks of
// Config.BlockSize, Config.Concurrency at a time, and that the zero values
// mean DefaultBlockSize (8 MiB, not the SDK's 1 MiB that caps a blob at
// 48.8 GiB) and DefaultConcurrency (not the SDK's core-count default).
func TestWriteBlockSizeAndConcurrency(t *testing.T) {
	ctx := t.Context()
	for name, tc := range map[string]struct {
		cfg       azure.Config
		body      int64
		wantSizes []int64
		peakLow   int
		peakHigh  int
	}{
		"default block size": {azure.Config{}, mib(9), []int64{mib(1), mib(8)}, 1, azure.DefaultConcurrency},
		"default concurrency": {
			azure.Config{BlockSize: azure.MinBlockSize}, mib(12),
			slices.Repeat([]int64{mib(1)}, 12), 2, azure.DefaultConcurrency,
		},
		"custom, one at a time": {azure.Config{BlockSize: mib(2), Concurrency: 1}, mib(5), []int64{mib(1), mib(2), mib(2)}, 1, 1},
		"custom, three at once": {azure.Config{BlockSize: mib(1), Concurrency: 3}, mib(9), slices.Repeat([]int64{mib(1)}, 9), 2, 3},
	} {
		t.Run(name, func(t *testing.T) {
			d, b := countingDisk(t, "big.bin", tc.cfg)
			if _, err := d.Write(ctx, "big.bin", bytes.NewReader(make([]byte, tc.body)), filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			sizes, peak := b.counted()
			if !slices.Equal(sizes, tc.wantSizes) {
				t.Errorf("staged blocks %v, want %v", sizes, tc.wantSizes)
			}
			if peak < tc.peakLow || peak > tc.peakHigh {
				t.Errorf("%d blocks in flight at once, want [%d, %d]", peak, tc.peakLow, tc.peakHigh)
			}
		})
	}
}

// TestBlockOptionsRefused proves New refuses a block size Azure or the SDK
// would not honour (over 4000 MiB, or under the SDK's 1 MiB minimum that
// it would silently raise) and a negative concurrency, and accepts the
// bounds themselves.
func TestBlockOptionsRefused(t *testing.T) {
	client, err := azblob.NewClientWithNoCredential("http://127.0.0.1:1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]azure.Config{
		"block under minimum": {BlockSize: azure.MinBlockSize - 1},
		"block over maximum":  {BlockSize: azure.MaxBlockSize + 1},
		"negative block":      {BlockSize: -1},
		"negative workers":    {Concurrency: -1},
	} {
		cfg.Client, cfg.Container = client, "c"
		if _, err := azure.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, size := range []int64{azure.MinBlockSize, azure.MaxBlockSize} {
		if _, err := azure.New(azure.Config{Client: client, Container: "c", BlockSize: size, Concurrency: 1}); err != nil {
			t.Errorf("BlockSize %d: %v", size, err)
		}
	}
}

// TestOpenerBlockParams proves block_size and concurrency in a DSN reach
// the upload, and that malformed or out-of-range values are refused.
func TestOpenerBlockParams(t *testing.T) {
	ctx := t.Context()
	b, url := countingServer(t, "big.bin")
	t.Setenv("AZ_TEST_BLOCKS", "AccountName=acct;AccountKey="+base64.StdEncoding.EncodeToString([]byte("k"))+";BlobEndpoint="+url)
	mux := filesystem.NewMux()
	_ = mux.Register(azure.Scheme, azure.Opener)
	base := "azblob://c?connection_env=AZ_TEST_BLOCKS"
	d, err := mux.Open(ctx, "az", base+"&block_size=2097152&concurrency=1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write(ctx, "big.bin", bytes.NewReader(make([]byte, mib(4))), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if sizes, peak := b.counted(); !slices.Equal(sizes, []int64{mib(2), mib(2)}) || peak != 1 {
		t.Errorf("DSN upload staged %v with %d at once, want two 2 MiB blocks one at a time", sizes, peak)
	}
	for _, bad := range []string{"&block_size=big", "&block_size=1024", "&concurrency=many", "&concurrency=-2"} {
		if _, err := mux.Open(ctx, "az", base+bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("Open(%s) = %v, want CodeInvalidArgument", bad, err)
		}
	}
	_ = mux.Register("azid", azure.TokenOpener(fakeToken{}))
	if _, err := mux.Open(ctx, "az", "azid://c?account=acct&block_size=16777216&concurrency=8"); err != nil {
		t.Errorf("token opener with block params: %v", err)
	}
	if _, err := mux.Open(ctx, "az", "azid://c?account=acct&block_size=big"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("token opener with a malformed block_size: %v", err)
	}
}
