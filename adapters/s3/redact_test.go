package s3_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// TestRedaction_DirectCalls pins that the customer key and the
// ScopedCredentials never show their secret through String or GoString called
// directly (fmt answers every verb through Format, so the verb loops
// elsewhere never reach those two), nor through fmt or slog.
func TestRedaction_DirectCalls(t *testing.T) {
	const secret = "print-very-secret-value"
	raw := bytes.Repeat([]byte{0x5c}, 32)
	key, err := fss3.NewCustomerKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	scoped := fss3.ScopedCredentials{AccessKeyID: "AKIDPRINT", SecretAccessKey: secret, SessionToken: secret, Bucket: "b", Prefix: "p"}
	var out bytes.Buffer
	fmt.Fprintf(&out, "%s\n%s\n%s\n%s\n", key.String(), key.GoString(), scoped.String(), scoped.GoString())
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		fmt.Fprintf(&out, verb+"\n", key)
		fmt.Fprintf(&out, verb+"\n", scoped)
	}
	slog.New(slog.NewJSONHandler(&out, nil)).Info("m", "key", key, "scoped", scoped)
	leaks := []string{secret, string(raw), base64.StdEncoding.EncodeToString(raw), fmt.Sprintf("%x", raw)}
	for _, leak := range leaks {
		if strings.Contains(out.String(), leak) {
			t.Fatalf("output leaks %q:\n%s", leak, out.String())
		}
	}
	if !strings.Contains(out.String(), "REDACTED") {
		t.Fatalf("no redaction marker:\n%s", out.String())
	}
}
