package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/sniff"
	"github.com/ubgo/filesystem/upload"
	"golang.org/x/net/html"
)

const (
	// maxTokenBytes caps one token of a page (a tag with its attributes,
	// a run of text): a page cannot make the tokenizer buffer more.
	maxTokenBytes = 64 << 10
	// maxFieldBytes caps each text field of a Preview.
	maxFieldBytes = 2048
	// MaxJSONLD is how many JSON-LD blocks of a page's head
	// Options.KeepMeta can offer Extract.
	MaxJSONLD = 8
	// metaLinkPrefix and metaJSONLDPrefix start the names KeepMeta is
	// offered for <link> tags and JSON-LD blocks.
	metaLinkPrefix   = "link:"
	metaJSONLDPrefix = "jsonld:"
	// jsonLDType is the type of a JSON-LD <script>.
	jsonLDType = "application/ld+json"
	// htmlType is the type a page must sniff as.
	htmlType = "text/html"
	// jsonType is the content type of stored previews.
	jsonType = "application/json"
)

// Preview is what a link preview shows of a page: its Open Graph (or
// Twitter card, or plain HTML) title, description and image, the image
// cached on the disk.
type Preview struct {
	// URL is the canonical URL of the page asked for.
	URL string `json:"url"`
	// Title, Description and SiteName come from og:title, og:description
	// and og:site_name, falling back to twitter: tags, <title> and
	// <meta name="description">. Each is at most 2048 bytes of the page's
	// own text, unescaped: an application must escape them where it
	// renders them, as any user input.
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	SiteName    string `json:"site_name,omitempty"`
	// ImageURL is the page's image, resolved against the page's final URL
	// and canonical (see Canonical): always an absolute http or https URL,
	// empty when the page names none or names anything else (ImageError
	// says so).
	ImageURL string `json:"image_url,omitempty"`
	// ImageKey, ImageType and ImageSize describe the image as cached on
	// the disk (see Cache.Get); ImageKey is empty when it was not cached.
	ImageKey  string `json:"image_key,omitempty"`
	ImageType string `json:"image_type,omitempty"`
	ImageSize int64  `json:"image_size,omitempty"`
	// ImageError is the code of the error that kept the image from the
	// cache (CodeRejected for a type the policy refuses, CodeNotFound, …),
	// empty when it was cached or the page names none. A preview with an
	// unusable image is still a preview.
	ImageError filesystem.Code `json:"image_error,omitempty"`
	// FetchedAt is when the page was fetched; Options.PreviewTTL counts
	// from it.
	FetchedAt time.Time `json:"fetched_at"`
	// Extra holds the fields Options.Extract returned; nil without it.
	Extra map[string]string `json:"extra,omitempty"`
	// OEmbed is what an allowlisted provider (Options.OEmbed) said about
	// the page; nil without Options.OEmbed, for a page no provider
	// answers for, or when asking failed (OEmbedError says why).
	OEmbed *OEmbed `json:"oembed,omitempty"`
	// OEmbedError is the code of the error that kept OEmbed empty
	// (CodeTooLarge, CodeRejected for a response that is not oEmbed
	// JSON, the fetch's code), empty when it was read or never asked.
	OEmbedError filesystem.Code `json:"oembed_error,omitempty"`
}

// LinkPreview returns the preview of the page at rawURL: the stored one
// while it is fresh (Options.PreviewTTL), else the page fetched again
// through the SSRF guard, parsed from at most MaxPageBytes and MaxTags,
// its image fetched (through the guard too) into the media cache, and the
// result stored. Errors: those of upload.Fetch for the page, and
// CodeRejected for a page that is not HTML.
func (c *Cache) LinkPreview(ctx context.Context, rawURL string) (Preview, error) {
	canon, err := Canonical(rawURL)
	if err != nil {
		return Preview{}, err
	}
	key := c.key(previewDir, canon) + previewExt
	if p, ok := c.storedPreview(ctx, key); ok {
		return p, nil
	}
	return c.previews.do(ctx, key, c.sharedTimeout(), func(ctx context.Context) (Preview, error) {
		p, err := c.fetchPreview(ctx, canon)
		if err != nil {
			return Preview{}, err
		}
		b, err := json.Marshal(p)
		if err != nil {
			return Preview{}, err
		}
		if _, err := c.disk.WriteBytes(ctx, key, b, filesystem.WriteOptions{ContentType: jsonType}); err != nil {
			return Preview{}, err
		}
		return p, nil
	})
}

// storedPreview returns the stored preview at key while it is fresh. A
// missing, unreadable or stale one is a miss, fetched and overwritten.
func (c *Cache) storedPreview(ctx context.Context, key string) (Preview, bool) {
	b, err := c.disk.ReadAll(ctx, key)
	if err != nil {
		return Preview{}, false
	}
	var p Preview
	if json.Unmarshal(b, &p) != nil {
		return Preview{}, false
	}
	if ttl := c.opts.PreviewTTL; ttl > 0 && c.opts.Now().Sub(p.FetchedAt) >= ttl {
		return Preview{}, false
	}
	return p, true
}

// fetchPreview fetches and parses the page at canon and caches its image.
func (c *Cache) fetchPreview(ctx context.Context, canon string) (Preview, error) {
	fo := c.opts.Fetch
	// A page's size says nothing about its <head>: read the start of a
	// large page rather than refusing it.
	fo.MaxSize = 0
	page, err := upload.Fetch(ctx, canon, fo)
	if err != nil {
		return Preview{}, err
	}
	defer func() { _ = page.Body.Close() }()
	body := bufio.NewReaderSize(io.LimitReader(page.Body, c.opts.MaxPageBytes), sniff.HeadSize)
	head, err := body.Peek(sniff.HeadSize)
	if err != nil && !errors.Is(err, io.EOF) {
		return Preview{}, filesystem.NewError(filesystem.CodeUnavailable, err)
	}
	if !sniff.Matches(sniff.Detect(head, ""), htmlType) {
		return Preview{}, filesystem.Errorf(filesystem.CodeRejected, "remote: the page is not HTML")
	}
	m, oembedHref := parseHead(body, c.opts.MaxTags, c.opts.KeepMeta)
	p := Preview{
		URL:         canon,
		Title:       m.first(metaOGTitle, metaTwitterTitle, metaHTMLTitle),
		Description: m.first(metaOGDescription, metaTwitterDescription, metaDescription),
		SiteName:    m.first(metaOGSiteName),
		FetchedAt:   c.opts.Now(),
	}
	if img := m.first(metaOGImage, metaOGImageURL, metaOGImageSecureURL, metaTwitterImage, metaTwitterImageSrc); img != "" {
		c.previewImage(ctx, &p, page.URL, img)
	}
	if len(c.opts.OEmbed) > 0 {
		if endpoint := c.oembedURL(canon, page.URL, oembedHref); endpoint != "" {
			c.previewOEmbed(ctx, &p, endpoint)
		}
	}
	if c.opts.Extract != nil {
		extra, err := c.opts.Extract(ctx, page.URL, maps.Clone(m))
		if err != nil {
			return Preview{}, err
		}
		p.Extra = extra
	}
	return p, nil
}

// previewImage resolves the image reference ref against the page's final
// URL and caches it, recording the outcome in p.
func (c *Cache) previewImage(ctx context.Context, p *Preview, pageURL, ref string) {
	base, err := url.Parse(pageURL)
	if err != nil {
		p.ImageError = filesystem.CodeInvalidArgument
		return
	}
	abs, err := base.Parse(ref)
	if err != nil {
		p.ImageError = filesystem.CodeInvalidArgument
		return
	}
	// Only an absolute http(s) URL is ever echoed: a page may name
	// "javascript:…" as its image, which an app rendering ImageURL as a
	// link or src would run.
	canon, err := Canonical(abs.String())
	if err != nil {
		p.ImageError = filesystem.CodeOf(err)
		return
	}
	p.ImageURL = canon
	e, err := c.Get(ctx, canon)
	if err != nil {
		p.ImageError = filesystem.CodeOf(err)
		return
	}
	p.ImageKey, p.ImageType, p.ImageSize = e.Key, e.ContentType, e.Size
}

// The tags a preview reads, by the name parseHead files them under.
const (
	metaOGTitle            = "og:title"
	metaOGDescription      = "og:description"
	metaOGSiteName         = "og:site_name"
	metaOGImage            = "og:image"
	metaOGImageURL         = "og:image:url"
	metaOGImageSecureURL   = "og:image:secure_url"
	metaTwitterTitle       = "twitter:title"
	metaTwitterDescription = "twitter:description"
	metaTwitterImage       = "twitter:image"
	metaTwitterImageSrc    = "twitter:image:src"
	metaDescription        = "description"
	// metaHTMLTitle files the <title> element's text.
	metaHTMLTitle = "<title>"
)

// headMeta maps a tag name to the first value a page gave it.
type headMeta map[string]string

// first returns the value of the first name that has one.
func (m headMeta) first(names ...string) string {
	for _, n := range names {
		if v := m[n]; v != "" {
			return v
		}
	}
	return ""
}

// set keeps the first non-empty value of name, trimmed and capped.
func (m headMeta) set(name, value string) { m.setCapped(name, value, maxFieldBytes) }

// setCapped is set with its own cap.
func (m headMeta) setCapped(name, value string, limit int) {
	value = strings.TrimSpace(value)
	if value == "" || m[name] != "" {
		return
	}
	if len(value) > limit {
		value = strings.ToValidUTF8(value[:limit], "")
	}
	m[name] = value
}

// reservedMeta reports whether a <meta> name may not be filed through
// KeepMeta: empty, or one parseHead files from another tag (the <title>
// text, a link rel, a JSON-LD block), which a page could otherwise spoof.
func reservedMeta(name string) bool {
	return name == "" || name == metaHTMLTitle || strings.HasPrefix(name, metaLinkPrefix) || strings.HasPrefix(name, metaJSONLDPrefix)
}

// builtinMeta reports whether name is one of the tags a preview reads
// itself, kept whatever Options.KeepMeta says.
func builtinMeta(name string) bool {
	return name == metaDescription || strings.HasPrefix(name, "og:") || strings.HasPrefix(name, "twitter:")
}

// parseHead reads <meta> and <title> tags from r until the head ends, the
// body starts, maxTags tags were read, a token outgrows maxTokenBytes, or
// the input ends, and the href of the first JSON oEmbed discovery link
// (at most maxFieldBytes, "" when none). keep, when set, is
// Options.KeepMeta: the other meta names, link rels and JSON-LD blocks it
// accepts are filed too. It never fails: a broken page yields what was
// read.
func parseHead(r io.Reader, maxTags int, keep func(string) bool) (m headMeta, oembedHref string) {
	m = headMeta{}
	jsonLD := 0
	z := html.NewTokenizer(r)
	z.SetMaxBuf(maxTokenBytes)
	for tags := 0; tags < maxTags; {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return m, oembedHref
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
			tags++
		default:
			continue
		}
		name, hasAttr := z.TagName()
		switch string(name) {
		case "body":
			return m, oembedHref
		case "head":
			if tt == html.EndTagToken {
				return m, oembedHref
			}
		case "link":
			if hasAttr && tt != html.EndTagToken {
				href := readLink(z, m, keep)
				if oembedHref == "" {
					oembedHref = href
				}
			}
		case "script":
			if keep != nil && tt == html.StartTagToken && jsonLD < MaxJSONLD && hasAttr && isJSONLD(z) {
				name := metaJSONLDPrefix + strconv.Itoa(jsonLD)
				jsonLD++
				if z.Next() == html.TextToken && keep(name) {
					m.setCapped(name, string(z.Text()), maxTokenBytes)
				}
			}
		case "title":
			if tt == html.StartTagToken && z.Next() == html.TextToken {
				m.set(metaHTMLTitle, string(z.Text()))
			}
		case "meta":
			if hasAttr {
				readMeta(z, m, keep)
			}
		}
	}
	return m, oembedHref
}

// isJSONLD reports whether the <script> tag being read is JSON-LD.
func isJSONLD(z *html.Tokenizer) bool {
	for {
		k, v, more := z.TagAttr()
		if string(k) == "type" {
			return strings.EqualFold(strings.TrimSpace(string(v)), jsonLDType)
		}
		if !more {
			return false
		}
	}
}

// readLink reads a <link> tag: it files "link:<rel>" for each of its
// rels that keep accepts, and returns its href when it is a JSON oEmbed
// discovery link (rel="alternate" type="application/json+oembed"), else
// "". An href over maxFieldBytes is no endpoint anyone allowlisted, and
// is not filed.
func readLink(z *html.Tokenizer, m headMeta, keep func(string) bool) string {
	var alternate, jsonType bool
	var href string
	var rels []string
	for {
		k, v, more := z.TagAttr()
		switch string(k) {
		case "rel":
			rels = strings.Fields(strings.ToLower(string(v)))
			alternate = slices.Contains(rels, "alternate")
		case "type":
			jsonType = strings.EqualFold(strings.TrimSpace(string(v)), oembedJSONType)
		case "href":
			href = strings.TrimSpace(string(v))
		}
		if !more {
			break
		}
	}
	if len(href) > maxFieldBytes {
		return ""
	}
	if keep != nil {
		for _, rel := range rels {
			if name := metaLinkPrefix + rel; keep(name) {
				m.set(name, href)
			}
		}
	}
	if !alternate || !jsonType {
		return ""
	}
	return href
}

// readMeta files a <meta property|name=… content=…> tag: one the preview
// reads itself, or one keep accepts.
func readMeta(z *html.Tokenizer, m headMeta, keep func(string) bool) {
	var name, content string
	for {
		k, v, more := z.TagAttr()
		switch string(k) {
		case "property", "name":
			if name == "" {
				name = strings.ToLower(string(v))
			}
		case "content":
			content = string(v)
		}
		if !more {
			break
		}
	}
	// Only the names a preview reads, or KeepMeta accepts, are kept, so
	// a page cannot fill the map with others.
	if builtinMeta(name) || (keep != nil && !reservedMeta(name) && keep(name)) {
		m.set(name, content)
	}
}
