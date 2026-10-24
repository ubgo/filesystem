// Package keynorm holds Unicode key normalisers for
// filesystem.WithKeyNormalizer. They live in their own module because
// they need golang.org/x/text, which the core does not import.
//
// The same visible name can arrive in two byte spellings: macOS file
// pickers send "café" decomposed (NFD: "e" + combining accent), Windows
// and Linux composed (NFC: "é"). Without normalisation they are two keys,
// and a file uploaded from one machine is "not found" from another.
//
//	disk, err := filesystem.NewDisk("media", drv, filesystem.WithKeyNormalizer(keynorm.NFC))
package keynorm

import (
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/ubgo/filesystem"
)

// NFC returns key in Unicode Normalization Form C, the composed form most
// systems and the web use, so its NFC and NFD spellings address one
// object. It changes nothing but the Unicode form: no case folding, no
// compatibility mapping (fullwidth letters stay fullwidth), so it can never
// turn a key into a different path. Invalid UTF-8 is CodeInvalidKey.
func NFC(key string) (string, error) {
	if !utf8.ValidString(key) {
		return "", filesystem.Errorf(filesystem.CodeInvalidKey, "key is not valid UTF-8")
	}
	return norm.NFC.String(key), nil
}
