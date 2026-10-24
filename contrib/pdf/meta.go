package pdf

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaxInfoChars bounds Info.Title and Info.Author in characters. They are
// written by whoever made the file and shown beside it, so they are cut
// to the length of a heading.
const MaxInfoChars = 255

// textEncoding is the encoding pdfinfo and pdftotext are asked to print
// in, so titles and text arrive as UTF-8 whatever the PDF stores.
const textEncoding = "UTF-8"

// pdfinfo report fields of the document information dictionary.
const (
	fieldTitle     = "Title"
	fieldAuthor    = "Author"
	fieldCreated   = "CreationDate"
	fieldEncrypted = "Encrypted"
)

// encryptedYes starts pdfinfo's Encrypted value for an encrypted file
// ("yes (print:yes copy:no change:no addNotes:no algorithm:RC4)").
const encryptedYes = "yes"

// isoDateLayouts are the forms of pdfinfo -isodates: Poppler prints the
// zone as "Z", "+05:30" or "+05" depending on what the file stores, and
// none at all when it stores none.
var isoDateLayouts = []string{time.RFC3339, "2006-01-02T15:04:05-07", "2006-01-02T15:04:05"}

// parseDocField reads one document information field of a pdfinfo report
// into info; other fields are ignored. The first Title and Author win:
// they come before every field a crafted title could imitate.
func parseDocField(info *Info, name, value string) {
	switch name {
	case fieldTitle:
		if info.Title == "" {
			info.Title = cleanInfoText(value)
		}
	case fieldAuthor:
		if info.Author == "" {
			info.Author = cleanInfoText(value)
		}
	case fieldCreated:
		info.Created = parseISODate(value)
	case fieldEncrypted:
		info.Encrypted = strings.HasPrefix(value, encryptedYes)
	}
}

// cleanInfoText makes untrusted document text safe to store and show:
// valid UTF-8, control characters replaced by spaces, trimmed and cut to
// MaxInfoChars characters.
func cleanInfoText(v string) string {
	v = strings.ToValidUTF8(v, "")
	v = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, v)
	v = strings.TrimSpace(v)
	if utf8.RuneCountInString(v) > MaxInfoChars {
		v = strings.TrimSpace(string([]rune(v)[:MaxInfoChars]))
	}
	return v
}

// parseISODate reads a date of pdfinfo -isodates, in UTC; zero when it
// is none of isoDateLayouts.
func parseISODate(v string) time.Time {
	for _, layout := range isoDateLayouts {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
