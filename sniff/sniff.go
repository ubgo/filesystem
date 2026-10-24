// Package sniff detects a file's MIME type from its first bytes, refined by
// its file name only where the bytes cannot tell.
//
// Why it exists: an upload's declared Content-Type and file extension are
// chosen by the client and cannot be trusted. Serving a client-declared
// "text/html" or "image/svg+xml" from the application's origin is a stored
// XSS. Detection reads the bytes; the extension is consulted only to refine
// a generic answer (plain text, octet-stream, zip) into a specific one from a
// fixed table, and never into a type a browser would execute.
package sniff

import (
	"bytes"
	"mime"
	"net/http"
	"path"
	"strings"
)

// HeadSize is how many leading bytes Detect needs; more are ignored. It
// matches net/http's sniffing window.
const HeadSize = 512

// MIME types this package returns or refines into. Callers compare against
// these constants rather than string literals.
const (
	OctetStream = "application/octet-stream"
	TextPlain   = "text/plain; charset=utf-8"
	TextXML     = "text/xml; charset=utf-8"
	Zip         = "application/zip"
	SVG         = "image/svg+xml"
	AVIF        = "image/avif"
	HEIC        = "image/heic"
	HEIF        = "image/heif"
	QuickTime   = "video/quicktime"
	MP4         = "video/mp4"
	M4A         = "audio/mp4"
	ThreeGPP    = "video/3gpp"
	CSV         = "text/csv; charset=utf-8"
	JSON        = "application/json"
	Markdown    = "text/markdown; charset=utf-8"
	DOCX        = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	XLSX        = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	PPTX        = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	EPUB        = "application/epub+zip"
)

// textRefinements maps an extension to the type plain text becomes. Only
// inert, non-executable text types appear here.
var textRefinements = map[string]string{
	".csv":  CSV,
	".json": JSON,
	".md":   Markdown,
}

// zipRefinements maps an extension to the type a zip container becomes:
// the office formats and EPUB are zip files.
var zipRefinements = map[string]string{
	".docx": DOCX,
	".xlsx": XLSX,
	".pptx": PPTX,
	".epub": EPUB,
}

// isoBrands maps an ISO base media "ftyp" brand to its MIME type. net/http
// recognises MP4 only when the box size fits the bytes it was given, which
// short heads from real uploads often do not, so the common brands are here
// too.
var isoBrands = map[string]string{
	"avif": AVIF,
	"avis": AVIF,
	"heic": HEIC,
	"heix": HEIC,
	"mif1": HEIF,
	"msf1": HEIF,
	"qt  ": QuickTime,
	"isom": MP4,
	"iso2": MP4,
	"mp41": MP4,
	"mp42": MP4,
	"avc1": MP4,
	"M4V ": MP4,
	"M4A ": M4A,
	"3gp4": ThreeGPP,
	"3gp5": ThreeGPP,
}

// Detect returns the MIME type of content whose first bytes are head (at
// most HeadSize are examined) and whose name is name (may be empty). The
// result is never empty: unknown binary content is OctetStream.
func Detect(head []byte, name string) string {
	if len(head) > HeadSize {
		head = head[:HeadSize]
	}
	if t, ok := isoBrand(head); ok {
		return t
	}
	detected := http.DetectContentType(head)
	if isSVG(head, detected) {
		return SVG
	}
	ext := strings.ToLower(path.Ext(name))
	switch detected {
	case TextPlain:
		if t, ok := textRefinements[ext]; ok {
			return t
		}
	case Zip:
		if t, ok := zipRefinements[ext]; ok {
			return t
		}
	}
	return detected
}

// Detector labels content the built-in detection does not know: given the
// first bytes (at most HeadSize) and the file name, it returns a MIME type,
// or "" when it does not recognise the content either. It is the hook for
// formats Go has no signature for (DICOM, STL, glTF, camera RAW, Parquet,
// 7z), so a strict upload allowlist can name them instead of allowing
// application/octet-stream. Like Detect, it must judge the bytes; the name
// is a hint only, because the client chooses it.
type Detector func(head []byte, name string) string

// DetectWith is Detect, extended by custom. custom is consulted only when
// Detect answers OctetStream, so it can label unknown content but never
// relabel a type Detect recognises: HTML stays HTML, a PNG stays a PNG.
// An empty or malformed answer (one mime.ParseMediaType refuses) keeps
// OctetStream, so a broken detector fails closed under a policy that does
// not allow octet-stream. A nil custom is plain Detect. Its answer is not
// otherwise trusted: callers still apply their deny rules to it, and an
// active type (IsActive) it returns is refused by upload policies and
// served as an attachment like any other.
func DetectWith(custom Detector, head []byte, name string) string {
	detected := Detect(head, name)
	if custom == nil || detected != OctetStream {
		return detected
	}
	if len(head) > HeadSize {
		head = head[:HeadSize]
	}
	t := strings.TrimSpace(custom(head, name))
	if _, _, err := mime.ParseMediaType(t); t == "" || err != nil {
		return OctetStream
	}
	return t
}

// isoBrand reads the major brand of an ISO base media file ("....ftypXXXX").
func isoBrand(head []byte) (string, bool) {
	const brandEnd = 12
	if len(head) < brandEnd || !bytes.Equal(head[4:8], []byte("ftyp")) {
		return "", false
	}
	t, ok := isoBrands[string(head[8:brandEnd])]
	return t, ok
}

// isSVG reports whether text or XML content is an SVG document. SVG is
// detected so that upload policies can refuse it: it can carry script.
func isSVG(head []byte, detected string) bool {
	if detected != TextPlain && detected != TextXML {
		return false
	}
	return bytes.Contains(bytes.ToLower(head), []byte("<svg"))
}

// Matches reports whether mime satisfies pattern. A pattern is an exact
// type ("image/png"), a wildcard subtype ("image/*") or "*/*". Parameters
// such as "; charset=utf-8" are ignored on both sides, and matching is case
// insensitive.
func Matches(mime, pattern string) bool {
	m := baseType(mime)
	p := baseType(pattern)
	if p == "*/*" {
		return m != ""
	}
	if prefix, ok := strings.CutSuffix(p, "/*"); ok {
		return strings.HasPrefix(m, prefix+"/")
	}
	return m == p
}

// MatchesAny reports whether mime satisfies at least one pattern.
func MatchesAny(mime string, patterns []string) bool {
	for _, p := range patterns {
		if Matches(mime, p) {
			return true
		}
	}
	return false
}

// baseType strips parameters and lowercases a MIME type.
func baseType(t string) string {
	base, _, _ := strings.Cut(t, ";")
	return strings.ToLower(strings.TrimSpace(base))
}

// ActiveTypes are the content types a browser renders with script: HTML,
// SVG, XML (which can carry XHTML), XSLT and JavaScript under its current
// and legacy names. Stored user content of these types is cross-site
// scripting waiting to happen when served inline from an application's
// origin; upload refuses them by default and fshttp serves them as
// downloads. The list names types; IsActive also treats every structured
// "+xml" type as active (activeXMLSuffix), which a list cannot express.
var ActiveTypes = []string{
	"text/html",
	"application/xhtml+xml",
	SVG,
	"text/xml",
	"application/xml",
	"text/xsl",
	"application/xslt+xml",
	"text/javascript",
	"application/javascript",
	"application/x-javascript",
	"application/ecmascript",
	"application/x-ecmascript",
	"text/ecmascript",
	"text/x-ecmascript",
	"text/x-javascript",
	"text/jscript",
	"text/livescript",
	"text/javascript1.0",
	"text/javascript1.1",
	"text/javascript1.2",
	"text/javascript1.3",
	"text/javascript1.4",
	"text/javascript1.5",
}

// activeXMLSuffix marks a structured XML type (RFC 6839): a browser parses
// any "*/*+xml" document as XML, and XHTML elements inside it run script,
// so every such type is active, not only the ones ActiveTypes names.
const activeXMLSuffix = "+xml"

// IsActive reports whether mime is one of ActiveTypes or any "+xml" type.
// Every decision that a type may run script in a browser goes through it.
func IsActive(mime string) bool {
	return MatchesAny(mime, ActiveTypes) || strings.HasSuffix(baseType(mime), activeXMLSuffix)
}

// Extension returns the conventional extension (with the dot) for a MIME
// type from a fixed table, or "" when there is none. Upload code uses it to
// name stored files by their detected type instead of the client's name.
func Extension(mime string) string {
	return extensions[baseType(mime)]
}

var extensions = map[string]string{
	"image/png":                    ".png",
	"image/jpeg":                   ".jpg",
	"image/gif":                    ".gif",
	"image/webp":                   ".webp",
	"image/bmp":                    ".bmp",
	"image/x-icon":                 ".ico",
	AVIF:                           ".avif",
	HEIC:                           ".heic",
	HEIF:                           ".heif",
	SVG:                            ".svg",
	"video/mp4":                    ".mp4",
	"video/webm":                   ".webm",
	"video/avi":                    ".avi",
	QuickTime:                      ".mov",
	"audio/mpeg":                   ".mp3",
	"audio/wave":                   ".wav",
	"audio/ogg":                    ".ogg",
	"audio/aiff":                   ".aiff",
	"application/ogg":              ".ogg",
	"application/pdf":              ".pdf",
	Zip:                            ".zip",
	"application/x-gzip":           ".gz",
	"application/x-rar-compressed": ".rar",
	"application/wasm":             ".wasm",
	"font/woff":                    ".woff",
	"font/woff2":                   ".woff2",
	"font/ttf":                     ".ttf",
	"font/otf":                     ".otf",
	"text/plain":                   ".txt",
	"text/html":                    ".html",
	"text/xml":                     ".xml",
	"text/csv":                     ".csv",
	JSON:                           ".json",
	"text/markdown":                ".md",
	DOCX:                           ".docx",
	XLSX:                           ".xlsx",
	PPTX:                           ".pptx",
	EPUB:                           ".epub",
}

// extensionAliases are extensions in common use besides the conventional
// one in extensions, and the choice for an extension two types share.
var extensionAliases = map[string]string{
	".jpeg":     "image/jpeg",
	".jpe":      "image/jpeg",
	".jfif":     "image/jpeg",
	".htm":      "text/html",
	".text":     "text/plain",
	".log":      "text/plain",
	".tif":      "image/tiff",
	".tiff":     "image/tiff",
	".m4a":      M4A,
	".m4v":      MP4,
	".3gp":      ThreeGPP,
	".ogg":      "audio/ogg", // also application/ogg; audio is what uploads mean by it
	".oga":      "audio/ogg",
	".markdown": "text/markdown",
}

// typesByExtension is the reverse of extensions, plus extensionAliases.
var typesByExtension = func() map[string]string {
	m := make(map[string]string, len(extensions)+len(extensionAliases))
	for t, ext := range extensions {
		m[ext] = t
	}
	for ext, t := range extensionAliases {
		m[ext] = t
	}
	return m
}()

// TypeByExtension returns the MIME type a file name's extension
// conventionally means, from a fixed table, or "" when the table does not
// know it. Unlike mime.TypeByExtension it reads no system files, so it
// answers the same on every machine. It never decides what a file is:
// Detect does that from the bytes.
func TypeByExtension(name string) string {
	return typesByExtension[strings.ToLower(path.Ext(name))]
}
