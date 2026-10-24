package filesystem

import (
	"math"
	"math/big"
	"strings"
)

// Size units for ParseSize. Decimal units are powers of 1000 and binary
// units powers of 1024, as the SI and IEC define them: "10MB" is
// 10,000,000 bytes and "10MiB" is 10,485,760. Units are case-sensitive,
// so "mb" (millibits, to the strict reader) is refused rather than guessed.
const (
	Byte int64 = 1
	KB   int64 = 1000
	MB         = 1000 * KB
	GB         = 1000 * MB
	TB         = 1000 * GB
	KiB  int64 = 1 << 10
	MiB  int64 = 1 << 20
	GiB  int64 = 1 << 30
	TiB  int64 = 1 << 40
)

// sizeUnits maps every accepted suffix to its multiplier. Longer suffixes
// are tried first so "KiB" is not read as "Ki" + "B".
var sizeUnits = []struct {
	suffix string
	bytes  int64
}{
	{"KiB", KiB}, {"MiB", MiB}, {"GiB", GiB}, {"TiB", TiB},
	{"KB", KB}, {"MB", MB}, {"GB", GB}, {"TB", TB},
	{"B", Byte},
}

// ParseSize reads a human size such as "10MB", "1.5GiB", "512KiB", "64B"
// or "1024" (bytes) into a byte count, for configuration values like an
// upload limit. It is strict: the number is a plain decimal (digits, at
// most one ".", no sign, exponent, separator or space), the unit is one of
// B, KB, MB, GB, TB (powers of 1000) or KiB, MiB, GiB, TiB (powers of 1024)
// written exactly so, nothing may follow it, and the result must be a
// whole number of bytes that fits an int64 ("1.5B" is refused). Anything
// else is CodeInvalidArgument, never a guess.
func ParseSize(s string) (int64, error) {
	number, unit := s, Byte
	for _, u := range sizeUnits {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			number, unit = n, u.bytes
			break
		}
	}
	if !plainDecimal(number) {
		return 0, Errorf(CodeInvalidArgument, "size %q is not a number followed by B, KB, MB, GB, TB, KiB, MiB, GiB or TiB", s)
	}
	r, _ := new(big.Rat).SetString(number) // plainDecimal guarantees it parses
	r.Mul(r, new(big.Rat).SetInt64(unit))
	if !r.IsInt() || !r.Num().IsInt64() {
		return 0, Errorf(CodeInvalidArgument, "size %q is not a whole number of bytes up to %d", s, int64(math.MaxInt64))
	}
	return r.Num().Int64(), nil
}

// plainDecimal reports whether s is digits with at most one "." between
// digits: "10", "1.5", never "", ".5", "5.", "1e3", "-1" or "1_000".
func plainDecimal(s string) bool {
	whole, frac, hasDot := strings.Cut(s, ".")
	return allDigits(whole) && (!hasDot || allDigits(frac))
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
