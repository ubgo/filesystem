package filesystem

import (
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

// KeyEncoder maps keys to names a backend can store and back. Some
// backends refuse characters a key may hold (SFTP servers on Windows refuse
// : * ? < > | ", Azure refuses a blob name ending in a dot); a driver for
// such a backend holds a KeyEncoder, encodes every key it sends and decodes
// every name it lists. Keys reaching and leaving the Disk are never
// encoded, so applications and other drivers see the same keys.
//
// DecodeKey(EncodeKey(k)) must return k for every clean key, and EncodeKey
// must not produce "/" or "..", so the encoded name keeps the key's
// segments.
type KeyEncoder interface {
	EncodeKey(key string) string
	DecodeKey(name string) (string, error)
}

// escapeMark starts an escape: '%' followed by two hex digits per byte of
// the character, as in URLs. A literal '%' is escaped too, so decoding is
// unambiguous.
const escapeMark = '%'

// escapeWidth is the length of one escape: the mark plus the two hex
// digits of one byte ("%3A"). DecodeKey reads escapes in steps of it.
const escapeWidth = 3

// KeyEscaper is the stock KeyEncoder: it percent-escapes (as "%3A") every
// character in Chars anywhere in a key, every character in SegmentEnd
// when it ends a segment (Windows and Azure refuse names ending in "." or
// " "), and '%' itself. The zero value escapes only '%'.
type KeyEscaper struct {
	Chars      string
	SegmentEnd string
}

// EncodeKey implements KeyEncoder.
func (e KeyEscaper) EncodeKey(key string) string {
	segs := strings.Split(key, separator)
	for i, seg := range segs {
		var b strings.Builder
		for j, r := range seg {
			last := j+utf8.RuneLen(r) == len(seg)
			if r == escapeMark || strings.ContainsRune(e.Chars, r) || (last && strings.ContainsRune(e.SegmentEnd, r)) {
				for _, c := range []byte(string(r)) {
					fmt.Fprintf(&b, "%c%02X", escapeMark, c)
				}
				continue
			}
			b.WriteRune(r)
		}
		segs[i] = b.String()
	}
	return strings.Join(segs, separator)
}

// DecodeKey implements KeyEncoder. It accepts only names EncodeKey
// produces: a malformed escape, an escape EncodeKey would not write ("%41"
// for "A", lower-case hex, "%2F" for "/"), or a raw character it would have
// escaped is CodeInvalidKey, so two names can never decode to one key and a
// name can never gain a segment.
func (e KeyEscaper) DecodeKey(name string) (string, error) {
	var out []byte
	for i := 0; i < len(name); i++ {
		if name[i] != escapeMark {
			out = append(out, name[i])
			continue
		}
		if i+escapeWidth > len(name) {
			return "", Errorf(CodeInvalidKey, "name has an incomplete escape")
		}
		c, err := hex.DecodeString(name[i+1 : i+escapeWidth])
		if err != nil {
			return "", Errorf(CodeInvalidKey, "name has an invalid escape %q", name[i:i+escapeWidth])
		}
		out = append(out, c[0])
		i += escapeWidth - 1
	}
	if key := string(out); e.EncodeKey(key) == name {
		return key, nil
	}
	return "", Errorf(CodeInvalidKey, "name %q was not written by this key encoder", name)
}
