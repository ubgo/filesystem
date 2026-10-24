package video

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
)

// TestHLSEncryptionLogValue: a structured log of the encryption names
// the key URI and never the key.
func TestHLSEncryptionLogValue(t *testing.T) {
	var buf bytes.Buffer
	key := bytes.Repeat([]byte{0xab}, HLSKeyBytes)
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("hls", "enc", HLSEncryption{Key: key, KeyURI: "https://keys.example/k"})
	out := buf.String()
	if strings.Contains(out, hex.EncodeToString(key)) || strings.Contains(out, string(key)) || !strings.Contains(out, "https://keys.example/k") || !strings.Contains(out, "[redacted]") {
		t.Fatalf("log line %s", out)
	}
}

// TestWriteKeyInfo: the key info file names the key URI, the key file
// (holding the key, readable by the owner only) and the IV in hex when
// set; a work directory that cannot be made fails it.
func TestWriteKeyInfo(t *testing.T) {
	key := bytes.Repeat([]byte{1}, HLSKeyBytes)
	iv := bytes.Repeat([]byte{2}, HLSKeyBytes)
	tmp := t.TempDir()
	info, err := HLS{Encryption: &HLSEncryption{Key: key, KeyURI: "k.key", IV: iv}}.writeKeyInfo(tmp)
	if err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(info)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(text), "\n"), "\n")
	if len(lines) != 3 || lines[0] != "k.key" || lines[2] != hex.EncodeToString(iv) {
		t.Fatalf("key info %q", text)
	}
	if got, err := os.ReadFile(lines[1]); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("key file %v, %v", got, err)
	}
	if fi, _ := os.Stat(lines[1]); fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v", fi.Mode().Perm())
	}
	if got, err := (HLS{}).writeKeyInfo(tmp); got != "" || err != nil {
		t.Errorf("no encryption = %q, %v", got, err)
	}
	if _, err := (HLS{Encryption: &HLSEncryption{Key: key, KeyURI: "k"}}).writeKeyInfo(filepath.Join(tmp, "missing")); err == nil {
		t.Error("a key info under a missing directory was written")
	}
}

// TestHLSPathRefusals: the plugin's entry point checks the options
// itself, and a work directory that cannot be made fails before FFmpeg
// runs; a missing work file and a missing work directory fail writing
// and listing the ladder.
func TestHLSPathRefusals(t *testing.T) {
	ctx := t.Context()
	write := func(context.Context, string, string, io.Reader) error {
		t.Fatal("nothing may be written")
		return nil
	}
	if err := (Tool{}).hlsPath(ctx, "in.mp4", Info{}, HLS{SegmentDuration: -1}, write); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("negative option = %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if err := (Tool{TempDir: missing}).hlsPath(ctx, "in.mp4", Info{}, HLS{}, write); !os.IsNotExist(err) {
		t.Errorf("missing TempDir = %v", err)
	}
	if err := writeHLSFile(ctx, filepath.Join(missing, "a.ts"), "a.ts", write); !os.IsNotExist(err) {
		t.Errorf("missing work file = %v", err)
	}
	if _, err := (HLS{}).files(missing, DefaultRenditions); !os.IsNotExist(err) {
		t.Errorf("missing work directory = %v", err)
	}
}
