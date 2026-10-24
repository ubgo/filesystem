package pickers

import "net/url"

// redactURLError removes the parts of a request URL that can be a
// credential from err when it is a *url.Error, which is what net/http
// returns for a failed request and url.Parse for a malformed URL, and
// whose text otherwise echoes the whole URL into every log: the query
// always (signatures, keys, expiries, upload-session IDs), user info, and
// the path too when keepPath refuses the URL's origin (pre-authenticated
// download and upload URLs carry their credential there). Any other error
// is returned unchanged.
func redactURLError(err error, keepPath func(origin string) bool) error {
	ue, ok := err.(*url.Error)
	if !ok {
		return err
	}
	safe := "[redacted URL]"
	if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
		origin := u.Scheme + "://" + u.Host
		safe = origin + "/[redacted path]"
		if keepPath(origin) {
			safe = origin + u.EscapedPath()
		}
	}
	return &url.Error{Op: ue.Op, URL: safe, Err: ue.Err}
}
