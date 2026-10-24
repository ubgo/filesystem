package gcs_test

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gcs"
	"github.com/ubgo/filesystem/filesystemtest"
)

// EnvTestDSN names a DSN of a real GCS bucket (gs://bucket?…). When set,
// the conformance suite runs against it, each case under a random prefix
// that is deleted afterwards. Credentials come from Application Default
// Credentials.
const EnvTestDSN = "FILESYSTEM_GCS_TEST_DSN"

func TestConformanceAgainstARealBucket(t *testing.T) {
	dsn := os.Getenv(EnvTestDSN)
	if dsn == "" {
		t.Skipf("set %s to run the suite against a real bucket", EnvTestDSN)
	}
	mux := filesystem.NewMux()
	_ = mux.Register(gcs.Scheme, gcs.Opener)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	filesystemtest.RunWith(t, func(t *testing.T) *filesystem.Disk {
		prefix := "conformance-" + strings.ToLower(rand.Text())
		d, err := mux.Open(t.Context(), "real", dsn+sep+filesystem.ParamPrefix+"="+prefix)
		if err != nil {
			t.Fatal(err)
		}
		root, err := mux.Open(t.Context(), "root", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = root.DeletePrefix(context.Background(), prefix) })
		return d
	}, conformanceOptions)
}
