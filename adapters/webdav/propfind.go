package webdav

import (
	"context"
	"encoding/xml"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/ubgo/filesystem"
)

// PROPFIND depths the driver uses.
const (
	depthZero = "0"
	depthOne  = "1"
)

// propfindBody asks for exactly the properties an Entry needs.
const propfindBody = `<?xml version="1.0" encoding="utf-8"?>` +
	`<d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/><d:getcontentlength/>` +
	`<d:getlastmodified/><d:getetag/><d:getcontenttype/></d:prop></d:propfind>`

// maxMultistatus caps a PROPFIND answer, so a hostile or broken server
// cannot exhaust memory. A depth-1 listing of a collection with a hundred
// thousand files fits.
const maxMultistatus = 64 << 20

// multistatus is the subset of RFC 4918's multistatus the driver reads.
type multistatus struct {
	Responses []struct {
		Href     string `xml:"DAV: href"`
		Propstat []struct {
			Status string `xml:"DAV: status"`
			Prop   struct {
				ResourceType struct {
					Collection *struct{} `xml:"DAV: collection"`
				} `xml:"DAV: resourcetype"`
				ContentLength string `xml:"DAV: getcontentlength"`
				LastModified  string `xml:"DAV: getlastmodified"`
				ETag          string `xml:"DAV: getetag"`
				ContentType   string `xml:"DAV: getcontenttype"`
			} `xml:"DAV: prop"`
		} `xml:"DAV: propstat"`
	} `xml:"DAV: response"`
}

// resource is one PROPFIND answer: a file's entry, or a collection.
type resource struct {
	entry filesystem.Entry
	dir   bool
}

// propfind asks for key (a collection when dir) at depth and returns the
// resources under the endpoint it names, keyed relative to it.
func (d *Driver) propfind(ctx context.Context, key string, dir bool, depth string) ([]resource, error) {
	h := http.Header{}
	h.Set("Depth", depth)
	h.Set("Content-Type", "application/xml; charset=utf-8")
	resp, err := d.do(ctx, "PROPFIND", d.urlOf(key, dir), strings.NewReader(propfindBody), int64(len(propfindBody)), h)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusMultiStatus {
		defer drain(resp)
		return nil, statusError(resp, "PROPFIND "+key)
	}
	defer func() { _ = resp.Body.Close() }()
	var ms multistatus
	if err := xml.NewDecoder(io.LimitReader(resp.Body, maxMultistatus)).Decode(&ms); err != nil {
		return nil, filesystem.NewError(filesystem.CodeUnavailable, err)
	}
	var out []resource
	for _, r := range ms.Responses {
		k, ok := d.keyOf(r.Href)
		if !ok {
			continue
		}
		var res resource
		res.entry.Key = k
		for _, ps := range r.Propstat {
			if !statusOK(ps.Status) {
				continue
			}
			p := ps.Prop
			if p.ResourceType.Collection != nil {
				res.dir = true
			}
			if n, err := strconv.ParseInt(strings.TrimSpace(p.ContentLength), 10, 64); err == nil {
				res.entry.Size = n
			}
			if t, err := http.ParseTime(strings.TrimSpace(p.LastModified)); err == nil {
				res.entry.ModTime = t.UTC()
			}
			if p.ETag != "" {
				res.entry.ETag = bareETag(p.ETag)
			}
			if p.ContentType != "" {
				res.entry.ContentType = strings.TrimSpace(p.ContentType)
			}
		}
		if res.entry.ContentType == "" && !res.dir {
			res.entry.ContentType = mime.TypeByExtension(path.Ext(k))
		}
		out = append(out, res)
	}
	return out, nil
}

// statusOK reports a propstat status line of 200 ("HTTP/1.1 200 OK").
func statusOK(line string) bool {
	f := strings.Fields(line)
	return len(f) >= 2 && f[1] == "200"
}

// keyOf turns an href (an absolute path, or a full URL) into a key relative
// to the endpoint, without a trailing slash. An href outside the endpoint,
// or one that is not a clean key, is dropped rather than trusted, so a
// server cannot make a listing name a key the Disk would refuse.
func (d *Driver) keyOf(href string) (string, bool) {
	u, err := url.Parse(href)
	if err != nil {
		return "", false
	}
	p := u.Path
	if p+"/" == d.base.Path {
		return "", true
	}
	if !strings.HasPrefix(p, d.base.Path) {
		return "", false
	}
	k := strings.TrimSuffix(strings.TrimPrefix(p, d.base.Path), "/")
	if k == "" {
		return "", true
	}
	if clean, err := filesystem.CleanKey(k); err != nil || clean != k {
		return "", false
	}
	return k, true
}

// bareETag drops the weak prefix and the quotes HTTP puts around an ETag:
// drivers report the bare value, which the Disk quotes when it serves it.
func bareETag(s string) string {
	s = strings.TrimPrefix(strings.TrimSpace(s), "W/")
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	return s
}
