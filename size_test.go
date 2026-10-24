package filesystem_test

import (
	"math"
	"strconv"
	"testing"

	"github.com/ubgo/filesystem"
)

func TestParseSize(t *testing.T) {
	ok := map[string]int64{
		"0":      0,
		"1024":   1024,
		"64B":    64,
		"10KB":   10_000,
		"10MB":   10_000_000,
		"2GB":    2_000_000_000,
		"1TB":    1_000_000_000_000,
		"512KiB": 512 << 10,
		"10MiB":  10 << 20,
		"1.5GiB": 3 << 29,
		"1TiB":   1 << 40,
		"0.5KB":  500,
		"007MB":  7_000_000,
		strconv.FormatInt(math.MaxInt64, 10) + "B": math.MaxInt64,
	}
	for in, want := range ok {
		got, err := filesystem.ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "MB", "10 MB", " 10MB", "10MB ", "10mb", "10Mb", "10M", "10MBs", "10KIB",
		"-1", "+1", "1e3", "1_000", "1,000", ".5MB", "5.MB", "1.2.3MB", "1.5B", "0.0001KB",
		"9223372036854775808", "8EiB", "9999999TiB",
	} {
		if _, err := filesystem.ParseSize(in); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("ParseSize(%q) = %v, want invalid_argument", in, err)
		}
	}
	if filesystem.MB != 1_000_000 || filesystem.MiB != 1<<20 || filesystem.TB != 1_000_000_000_000 {
		t.Error("unit constants")
	}
}
