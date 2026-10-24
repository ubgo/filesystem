package cloudinary

import (
	"encoding/hex"
	"strings"

	"github.com/ubgo/filesystem"
)

// idEscape starts an escaped byte in a public ID. Cloudinary refuses "%"
// (and ? & # \ < > +), so filesystem.KeyEscaper's percent escapes cannot
// be used.
const idEscape = '~'

// idSafe reports a byte stored as itself in a public ID segment.
func idSafe(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
}

// idSafeString reports whether every byte of s is idSafe.
func idSafeString(s string) bool {
	for i := 0; i < len(s); i++ {
		if !idSafe(s[i]) {
			return false
		}
	}
	return true
}

// encodeKey maps a clean key to a public ID: "/" separates folders, every
// other byte outside [A-Za-z0-9._-] becomes "~XX" (upper-case hex).
func encodeKey(key string) string {
	var b strings.Builder
	for i := 0; i < len(key); i++ {
		switch c := key[i]; {
		case c == '/' || idSafe(c):
			b.WriteByte(c)
		default:
			b.WriteByte(idEscape)
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

// decodeKey reverses encodeKey. It accepts only public IDs encodeKey
// writes, so two IDs never decode to one key.
func decodeKey(id string) (string, error) {
	var out []byte
	for i := 0; i < len(id); i++ {
		if id[i] != idEscape {
			out = append(out, id[i])
			continue
		}
		if i+2 >= len(id) {
			return "", filesystem.Errorf(filesystem.CodeInvalidKey, "cloudinary: public ID has an incomplete escape")
		}
		c, err := hex.DecodeString(id[i+1 : i+3])
		if err != nil {
			return "", filesystem.Errorf(filesystem.CodeInvalidKey, "cloudinary: public ID has an invalid escape")
		}
		out = append(out, c[0])
		i += 2
	}
	if key := string(out); encodeKey(key) == id {
		return key, nil
	}
	return "", filesystem.Errorf(filesystem.CodeInvalidKey, "cloudinary: public ID %q was not written by this driver", id)
}
