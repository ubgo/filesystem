package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/upload"
)

const (
	// DefaultMaxOEmbedBytes is how much of an oEmbed response LinkPreview
	// reads (Options.MaxOEmbedBytes): a real one is a few KiB.
	DefaultMaxOEmbedBytes int64 = 64 << 10
	// maxOEmbedHTMLBytes caps OEmbed.HTML, the embed code; an iframe tag
	// is a few hundred bytes.
	maxOEmbedHTMLBytes = 16 << 10
	// oembedJSONType is the type a discovery link must announce: only the
	// JSON format is read.
	oembedJSONType = "application/json+oembed"
	// oembedFormatPlaceholder is the spec's endpoint placeholder for the
	// response format.
	oembedFormatPlaceholder = "{format}"
	// oembedWildcard is the spec's wildcard in a URL scheme.
	oembedWildcard = "*"
)

// OEmbed types (the response's "type"): the closed set the spec defines.
// A response of any other type is dropped.
const (
	OEmbedPhoto = "photo"
	OEmbedVideo = "video"
	OEmbedLink  = "link"
	OEmbedRich  = "rich"
)

// oembedTypes lists every OEmbed type.
var oembedTypes = []string{OEmbedPhoto, OEmbedVideo, OEmbedLink, OEmbedRich}

// OEmbedProvider is one oEmbed provider the application trusts: LinkPreview
// asks no other endpoint. It is the application's configuration, never a
// page's or a user's input.
type OEmbedProvider struct {
	// Schemes are the page URLs the provider answers for, in the oEmbed
	// spec's form with "*" wildcards ("https://www.youtube.com/watch*",
	// "https://*.vimeo.com/*"). A page URL matching one is sent to
	// Endpoint without reading the page's discovery link. Empty means the
	// provider is reached only through discovery.
	Schemes []string
	// Endpoint is the provider's oEmbed endpoint, an absolute http or
	// https URL ("https://www.youtube.com/oembed"; the spec's "{format}"
	// placeholder becomes "json"). The page's URL goes in its url
	// parameter and format=json is added. A page's discovery link is
	// followed only when it names this endpoint (its query aside).
	Endpoint string
}

// OEmbed is what an allowlisted provider said about a page: only the
// spec's fields, each typed and capped, every URL an absolute http or
// https URL (or dropped). Like the other preview fields they are the
// provider's text: escape them where they are rendered.
type OEmbed struct {
	// Type is one of OEmbedPhoto, OEmbedVideo, OEmbedLink, OEmbedRich.
	Type string `json:"type"`
	// Title, AuthorName and ProviderName are at most 2048 bytes each.
	Title        string `json:"title,omitempty"`
	AuthorName   string `json:"author_name,omitempty"`
	AuthorURL    string `json:"author_url,omitempty"`
	ProviderName string `json:"provider_name,omitempty"`
	ProviderURL  string `json:"provider_url,omitempty"`
	// URL is a photo's image.
	URL string `json:"url,omitempty"`
	// HTML is the embed code of a video or rich type, at most 16 KiB of
	// the provider's markup (dropped when longer, never cut). It is
	// markup from a third party: render it only inside a sandboxed iframe
	// (or not at all), never into the application's own page.
	HTML string `json:"html,omitempty"`
	// Width and Height are the embed's size in pixels.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// ThumbnailURL, ThumbnailWidth and ThumbnailHeight describe the
	// provider's thumbnail. It is not fetched into the cache: call
	// Cache.Get with it to serve it from there.
	ThumbnailURL    string `json:"thumbnail_url,omitempty"`
	ThumbnailWidth  int    `json:"thumbnail_width,omitempty"`
	ThumbnailHeight int    `json:"thumbnail_height,omitempty"`
}

// oembedResponse is the spec's response as decoded. Sizes are numbers or,
// in the wild, strings ("480", "auto"), so they are read raw and typed by
// size; every other field the provider sent is dropped by the decoder.
type oembedResponse struct {
	Type            string          `json:"type"`
	Title           string          `json:"title"`
	AuthorName      string          `json:"author_name"`
	AuthorURL       string          `json:"author_url"`
	ProviderName    string          `json:"provider_name"`
	ProviderURL     string          `json:"provider_url"`
	URL             string          `json:"url"`
	HTML            string          `json:"html"`
	Width           json.RawMessage `json:"width"`
	Height          json.RawMessage `json:"height"`
	ThumbnailURL    string          `json:"thumbnail_url"`
	ThumbnailWidth  json.RawMessage `json:"thumbnail_width"`
	ThumbnailHeight json.RawMessage `json:"thumbnail_height"`
}

// checkProviders refuses a provider without a usable endpoint or with an
// empty scheme.
func checkProviders(ps []OEmbedProvider) error {
	for _, p := range ps {
		if _, err := endpointBase(p.Endpoint); err != nil {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "remote: oEmbed endpoint %q is not an absolute http or https URL", p.Endpoint)
		}
		if slices.Contains(p.Schemes, "") {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "remote: empty oEmbed scheme for %q", p.Endpoint)
		}
	}
	return nil
}

// endpointBase is an endpoint's canonical form without its query: what a
// discovery link is compared by.
func endpointBase(endpoint string) (string, error) {
	u, err := url.Parse(strings.ReplaceAll(endpoint, oembedFormatPlaceholder, "json"))
	if err != nil {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "remote: invalid URL")
	}
	u.RawQuery, u.ForceQuery = "", false
	return Canonical(u.String())
}

// matchScheme reports whether rawURL matches an oEmbed URL scheme, "*"
// matching any run of characters. The scheme and host are matched apart
// from the rest, so a wildcard in the host ("https://*.vimeo.com/*") can
// never reach into the path: "https://evil.example/.vimeo.com/x" does not
// match it.
func matchScheme(scheme, rawURL string) bool {
	sh, sp := splitAuthority(scheme)
	uh, up := splitAuthority(rawURL)
	return matchGlob(sh, uh) && matchGlob(sp, up)
}

// splitAuthority cuts u after its scheme and host: "https://a.b/c?d"
// gives "https://a.b" and "/c?d".
func splitAuthority(u string) (string, string) {
	_, rest, _ := strings.Cut(u, "://")
	at := len(u) - len(rest)
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		return u[:at+i], u[at+i:]
	}
	return u, ""
}

// matchGlob reports whether s matches pattern, "*" matching any run of
// characters and everything else itself.
func matchGlob(pattern, s string) bool {
	parts := strings.Split(pattern, oembedWildcard)
	rest, ok := strings.CutPrefix(s, parts[0])
	if !ok {
		return false
	}
	last := len(parts) - 1
	if last == 0 {
		return rest == ""
	}
	for _, p := range parts[1:last] {
		i := strings.Index(rest, p)
		if i < 0 {
			return false
		}
		rest = rest[i+len(p):]
	}
	return strings.HasSuffix(rest, parts[last])
}

// oembedURL is the endpoint to ask about the page at canon: that of the
// first provider whose scheme matches canon, else the page's discovery
// link (href, resolved against the page's final URL) when it names an
// allowlisted endpoint, else "" (no oEmbed for this page).
func (c *Cache) oembedURL(canon, pageURL, href string) string {
	for _, p := range c.opts.OEmbed {
		if slices.ContainsFunc(p.Schemes, func(s string) bool { return matchScheme(s, canon) }) {
			u, err := url.Parse(strings.ReplaceAll(p.Endpoint, oembedFormatPlaceholder, "json"))
			if err != nil {
				return ""
			}
			q := u.Query()
			q.Set("url", canon)
			q.Set("format", "json")
			u.RawQuery = q.Encode()
			return u.String()
		}
	}
	if href == "" {
		return ""
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return ""
	}
	link, err := base.Parse(href)
	if err != nil {
		return ""
	}
	got, err := endpointBase(link.String())
	if err != nil {
		return ""
	}
	for _, p := range c.opts.OEmbed {
		if want, err := endpointBase(p.Endpoint); err == nil && want == got {
			return link.String()
		}
	}
	return ""
}

// previewOEmbed asks endpoint about the page and records the outcome in
// p: OEmbed on success, else OEmbedError. Like the image, a failed oEmbed
// leaves the preview a preview.
func (c *Cache) previewOEmbed(ctx context.Context, p *Preview, endpoint string) {
	o, err := c.fetchOEmbed(ctx, endpoint)
	if err != nil {
		p.OEmbedError = filesystem.CodeOf(err)
		return
	}
	p.OEmbed = o
}

// fetchOEmbed fetches endpoint through the SSRF guard, redirects
// included, reads at most MaxOEmbedBytes, and keeps the typed fields.
func (c *Cache) fetchOEmbed(ctx context.Context, endpoint string) (*OEmbed, error) {
	fo := c.opts.Fetch
	fo.MaxSize = c.opts.MaxOEmbedBytes
	res, err := upload.Fetch(ctx, endpoint, fo)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(res.Body, c.opts.MaxOEmbedBytes+1))
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeUnavailable, err)
	}
	if int64(len(b)) > c.opts.MaxOEmbedBytes {
		return nil, filesystem.Errorf(filesystem.CodeTooLarge, "remote: oEmbed response larger than %d bytes", c.opts.MaxOEmbedBytes)
	}
	var r oembedResponse
	if json.Unmarshal(b, &r) != nil {
		return nil, filesystem.Errorf(filesystem.CodeRejected, "remote: the oEmbed response is not JSON")
	}
	if !slices.Contains(oembedTypes, r.Type) {
		return nil, filesystem.Errorf(filesystem.CodeRejected, "remote: unknown oEmbed type")
	}
	o := &OEmbed{
		Type: r.Type, Title: capField(r.Title), AuthorName: capField(r.AuthorName), ProviderName: capField(r.ProviderName),
		AuthorURL: httpURL(r.AuthorURL), ProviderURL: httpURL(r.ProviderURL), URL: httpURL(r.URL), ThumbnailURL: httpURL(r.ThumbnailURL),
		Width: size(r.Width), Height: size(r.Height), ThumbnailWidth: size(r.ThumbnailWidth), ThumbnailHeight: size(r.ThumbnailHeight),
	}
	if len(r.HTML) <= maxOEmbedHTMLBytes {
		o.HTML = strings.ToValidUTF8(r.HTML, "")
	}
	return o, nil
}

// capField trims s and caps it like a page's text field.
func capField(s string) string {
	m := headMeta{}
	m.set("v", s)
	return m["v"]
}

// httpURL is s in canonical form when it is an absolute http or https
// URL, else "": a provider may name "javascript:…", which an application
// rendering the field as a link would run.
func httpURL(s string) string {
	if s == "" {
		return ""
	}
	canon, err := Canonical(s)
	if err != nil {
		return ""
	}
	return canon
}

// maxOEmbedSize bounds a size field: larger is no real embed.
const maxOEmbedSize = 1 << 16

// size reads a pixel size, a JSON number or a string holding one; 0 when
// it is not a whole number in 1..maxOEmbedSize.
func size(raw json.RawMessage) int {
	v, err := strconv.Atoi(strings.Trim(string(raw), `"`))
	if err != nil || v < 1 || v > maxOEmbedSize {
		return 0
	}
	return v
}
