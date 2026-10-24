// Package pickers imports a file a user chose in a cloud picker (the
// Google Drive Picker, the Dropbox Chooser, the OneDrive and SharePoint
// file picker) onto a disk. The browser hands the server what the picker
// returned, the file's ID and an access token for it; Import fetches the
// file from the vendor's API and stores it with upload.Receive, so the
// upload policy (size, detected type, names, keys, hooks) applies exactly
// as it does to a direct upload.
//
// The token is the user's credential. Import sends it only to the API
// origins the Source names, never to a host a redirect leads to (OneDrive
// downloads redirect to a pre-authenticated host, which gets no token),
// and every connection goes through upload.FetchOptions' guarded client,
// so neither a vendor answer nor a configured endpoint can reach an
// internal address. Pick prints and logs with the token redacted.
package pickers

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/upload"
)

// Pick is what a picker returned in the browser, posted to the server.
type Pick struct {
	// ID is the vendor's file ID: a Drive file ID, a Dropbox "id:…", a
	// OneDrive item ID.
	ID string
	// DriveID is the drive holding a OneDrive or SharePoint item, as the
	// picker reports it; empty means the token owner's own drive. Other
	// sources ignore it.
	DriveID string
	// Token is the OAuth access token the picker used (or the app's own
	// token for the user), sent only to the source's API.
	Token string
}

// String implements fmt.Stringer with the token redacted.
func (p Pick) String() string {
	tok := `""`
	if p.Token != "" {
		tok = "[redacted]"
	}
	return fmt.Sprintf("pickers.Pick{ID: %q, DriveID: %q, Token: %s}", p.ID, p.DriveID, tok)
}

// GoString implements fmt.GoStringer like String.
func (p Pick) GoString() string { return p.String() }

// Format implements fmt.Formatter, so no verb prints the token.
func (p Pick) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, p.String()) }

// LogValue implements slog.LogValuer with the token redacted.
func (p Pick) LogValue() slog.Value {
	return slog.GroupValue(slog.String("id", p.ID), slog.String("drive_id", p.DriveID), slog.Bool("token", p.Token != ""))
}

// File is a picked file's metadata.
type File struct {
	// Name is the vendor's file name, for upload.Receive's display name.
	Name string
	// Size is the length in bytes, or -1 when the vendor does not say.
	Size int64
}

// Source fetches picked files from one vendor. Its requests go out
// through c, which adds the pick's token to requests for Origins and to
// nothing else; a Source never sets credentials itself.
type Source interface {
	// Origins lists the scheme://host values the token may be sent to.
	Origins() []string
	// Stat returns the picked file's metadata; a file that has no bytes
	// (a Google Doc) is CodeUnsupported.
	Stat(ctx context.Context, c *http.Client, p Pick) (File, error)
	// Open streams the picked file's bytes; the caller closes them.
	Open(ctx context.Context, c *http.Client, p Pick) (io.ReadCloser, error)
}

// Authorizer is a Source whose vendor takes the token another way than
// "Authorization: Bearer <token>" (a custom header, another scheme).
// Authorize sets it on h, the headers of a request to one of the
// source's Origins; requests to any other host never reach it, and keep
// no Authorization header. A Source without the method gets Bearer.
type Authorizer interface {
	Authorize(h http.Header, token string)
}

// Import fetches the file p names from src and stores it on disk under
// policy pol (see upload.Receive): the type is detected from the bytes, a
// size the vendor reports over pol.MaxSize is refused before the download,
// and the body is capped as it streams. o configures the guarded client
// (timeout, redirects, AllowAddr for a known internal server); its
// MaxSize is ignored in favour of pol.MaxSize.
func Import(ctx context.Context, disk *filesystem.Disk, src Source, p Pick, pol upload.Policy, o upload.FetchOptions) (upload.Result, error) {
	if err := pol.Validate(); err != nil {
		return upload.Result{}, err
	}
	if p.ID == "" || p.Token == "" {
		return upload.Result{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "pickers: a pick needs an ID and a token")
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = upload.DefaultFetchTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := client(src, p.Token, o)
	f, err := src.Stat(ctx, c, p)
	if err != nil {
		return upload.Result{}, err
	}
	if f.Size > pol.MaxSize {
		return upload.Result{}, filesystem.Errorf(filesystem.CodeTooLarge, "pickers: the picked file is %d bytes, over the limit of %d", f.Size, pol.MaxSize)
	}
	body, err := src.Open(ctx, c, p)
	if err != nil {
		return upload.Result{}, err
	}
	defer func() { _ = body.Close() }()
	return upload.Receive(ctx, disk, body, f.Name, pol)
}

// client is o's guarded client with a transport adding the token for the
// source's origins only: as a Bearer header, or as the source's
// Authorizer says.
func client(src Source, token string, o upload.FetchOptions) *http.Client {
	c := o.Client()
	origins := map[string]bool{}
	for _, raw := range src.Origins() {
		if u, err := url.Parse(raw); err == nil {
			origins[u.Scheme+"://"+u.Host] = true
		}
	}
	set := func(h http.Header) { h.Set("Authorization", "Bearer "+token) }
	if a, ok := src.(Authorizer); ok {
		set = func(h http.Header) { a.Authorize(h, token) }
	}
	c.Transport = tokenTransport{next: c.Transport, origins: origins, set: set}
	return c
}

// tokenTransport adds the token to requests for origins and strips any
// Authorization header from requests to other hosts.
type tokenTransport struct {
	next    http.RoundTripper
	origins map[string]bool
	// set is a closure over the token; fmt prints a func as an address.
	set func(http.Header)
}

func (t tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	if t.origins[req.URL.Scheme+"://"+req.URL.Host] {
		t.set(r.Header)
	} else {
		r.Header.Del("Authorization")
	}
	return t.next.RoundTrip(r)
}

// endpointOr returns raw without a trailing slash, or def when raw is
// empty.
func endpointOr(raw, def string) string {
	if raw == "" {
		return def
	}
	return strings.TrimSuffix(raw, "/")
}

// statusError classifies a vendor's error answer; missing lists further
// statuses that mean the file is not there (Dropbox answers 409).
func statusError(resp *http.Response, what string, missing []int) error {
	cause := fmt.Errorf("pickers: %s: %s", what, resp.Status)
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone || slices.Contains(missing, resp.StatusCode):
		return filesystem.NewError(filesystem.CodeNotFound, cause)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return filesystem.NewError(filesystem.CodePermission, cause)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
		return filesystem.NewError(filesystem.CodeUnavailable, cause)
	}
	return filesystem.NewError(filesystem.CodeInvalidArgument, cause)
}

// send runs one request and classifies a non-2xx answer (see
// statusError); the caller closes a 2xx response's body.
func send(c *http.Client, req *http.Request, what string, missing ...int) (*http.Response, error) {
	resp, err := c.Do(req)
	if err != nil {
		// No path or query of any URL survives into the error: OneDrive's
		// download redirect carries its credential in both.
		err = redactURLError(err, func(string) bool { return false })
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		return nil, filesystem.NewError(filesystem.CodeUnavailable, err)
	}
	if resp.StatusCode/100 != 2 {
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		return nil, statusError(resp, what, missing)
	}
	return resp, nil
}

const maxErrorBody = 8 << 10

// maxJSONBody caps what is read of a JSON answer, far above the largest
// page a vendor sends for one file, so a hostile or broken server cannot make the
// import buffer without bound.
const maxJSONBody = 16 << 20
