package keynorm_test

import (
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/keynorm"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/memory"
)

const (
	nfc = "café.txt"  // é as one code point
	nfd = "café.txt" // e + combining acute accent
)

func TestNFC(t *testing.T) {
	if nfc == nfd {
		t.Fatal("test spellings are identical")
	}
	for _, in := range []string{nfc, nfd} {
		if got, err := keynorm.NFC(in); err != nil || got != nfc {
			t.Errorf("NFC(%q) = %q, %v", in, got, err)
		}
	}
	// No compatibility mapping: fullwidth dots stay, so no ".." appears.
	if got, _ := keynorm.NFC("．．"); got != "．．" {
		t.Errorf("NFC changed fullwidth dots: %q", got)
	}
	if _, err := keynorm.NFC("a\xffb"); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("invalid UTF-8: %v", err)
	}
}

// NFC and NFD spellings of one name address one object on a disk with the
// normaliser, on every backend; without it they are two keys.
func TestOneObjectForBothSpellings(t *testing.T) {
	ctx := t.Context()
	drv, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = drv.Close() })
	for name, d := range map[string]filesystem.Driver{"memory": memory.New(), "local": drv} {
		disk, err := filesystem.NewDisk("m", d, filesystem.WithKeyNormalizer(keynorm.NFC))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := disk.WriteBytes(ctx, "menu/"+nfd, []byte("decomposed upload"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		got, err := disk.ReadAll(ctx, "menu/"+nfc)
		if err != nil || string(got) != "decomposed upload" {
			t.Errorf("%s: read by NFC: %q, %v", name, got, err)
		}
		for e, err := range disk.List(ctx, "menu", filesystem.ListOptions{}) {
			if err != nil || e.Key != "menu/"+nfc {
				t.Errorf("%s: listed %q, %v", name, e.Key, err)
			}
		}
	}
	plain, _ := filesystem.NewDisk("p", memory.New())
	_, _ = plain.WriteBytes(ctx, nfd, []byte("x"), filesystem.WriteOptions{})
	if ok, _ := plain.Exists(ctx, nfc); ok {
		t.Error("without the normaliser the spellings should be two keys")
	}
}
