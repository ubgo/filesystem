package upload

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"github.com/ubgo/filesystem"
)

// Proxy constants of FetchOptions.Proxy.
const (
	// proxyScheme is the only proxy scheme the tunnel speaks: CONNECT over
	// plain TCP to the proxy.
	proxyScheme = "http"
	// proxyDefaultPort is the port of a proxy URL that names none.
	proxyDefaultPort = "80"
	// headerProxyAuthorization carries the proxy URL's user:password.
	headerProxyAuthorization = "Proxy-Authorization"
)

// checkProxy refuses a proxy URL the tunnel cannot use: anything but
// "http://host[:port]" (an optional user:password, no path, query or
// fragment).
func checkProxy(p *url.URL) error {
	if p == nil {
		return nil
	}
	if p.Scheme != proxyScheme || p.Hostname() == "" || (p.Path != "" && p.Path != "/") || p.RawQuery != "" || p.Fragment != "" {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "upload: the fetch proxy must be http://host:port")
	}
	return nil
}

// tunnel is the DialContext of a fetch through FetchOptions.Proxy. It
// resolves addr's host itself, keeps the addresses the SSRF guard allows
// (refusing the connection when none is), and asks the proxy to CONNECT
// to each in turn, so the destination the guard checked is the
// one the proxy reaches: the proxy is never handed a name to resolve.
func (o FetchOptions) tunnel(ctx context.Context, _, addr string) (net.Conn, error) {
	if err := checkProxy(o.Proxy); err != nil {
		return nil, err
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	targets, err := o.resolveAllowed(ctx, host)
	if err != nil {
		return nil, err
	}
	proxyAddr := o.Proxy.Host
	if o.Proxy.Port() == "" {
		proxyAddr = net.JoinHostPort(o.Proxy.Hostname(), proxyDefaultPort)
	}
	// Like a direct dial, try each allowed address in turn (a name often
	// resolves to an IPv6 and an IPv4 address, and only one may answer);
	// a proxy's refusal (4xx) is final.
	for _, target := range targets {
		conn, dialErr := (&net.Dialer{Timeout: fetchDialTimeout}).DialContext(ctx, "tcp", proxyAddr)
		if dialErr != nil {
			return nil, dialErr
		}
		tunneled, connErr := o.connect(ctx, conn, net.JoinHostPort(target.String(), port))
		if connErr == nil {
			return tunneled, nil
		}
		_ = conn.Close()
		err = connErr
		if filesystem.CodeOf(connErr) == filesystem.CodeInvalidArgument {
			break
		}
	}
	return nil, err
}

// resolveAllowed returns the addresses of host the guard allows, in the
// resolver's order; an IP literal is checked as it is. No allowed address
// is errBlocked, the same refusal a direct dial gives.
func (o FetchOptions) resolveAllowed(ctx context.Context, host string) ([]netip.Addr, error) {
	addrs := []netip.Addr{}
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = append(addrs, a)
	} else {
		resolved, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		addrs = resolved
	}
	allowed := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if o.allowed(a) {
			allowed = append(allowed, a.Unmap())
		}
	}
	switch {
	case len(allowed) > 0:
		return allowed, nil
	case len(addrs) == 0:
		return nil, fmt.Errorf("%w: %s resolves to no address", errBlocked, host)
	}
	return nil, fmt.Errorf("%w: %s", errBlocked, addrs[0])
}

// connect sends CONNECT target over conn and waits for the proxy's 2xx,
// within ctx's deadline or fetchDialTimeout, whichever comes first. A proxy refusing (4xx, as an egress policy
// does) is CodeInvalidArgument, like the guard's own refusal; any other
// answer is CodeUnavailable.
func (o FetchOptions) connect(ctx context.Context, conn net.Conn, target string) (net.Conn, error) {
	// The exchange is bounded even when ctx has no deadline (the
	// transport dials with a context of its own): a proxy that accepts
	// and never answers fails the dial instead of holding it.
	deadline := time.Now().Add(fetchDialTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)
	defer func() { _ = conn.SetDeadline(noDeadline) }()
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: http.Header{}}
	if u := o.Proxy.User; u != nil {
		pass, _ := u.Password()
		req.Header.Set(headerProxyAuthorization, "Basic "+base64.StdEncoding.EncodeToString([]byte(u.Username()+":"+pass)))
	}
	if err := req.Write(conn); err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
	case resp.StatusCode >= 400 && resp.StatusCode <= 499:
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "upload: the fetch proxy refused the connection (%d)", resp.StatusCode)
	default:
		return nil, filesystem.Errorf(filesystem.CodeUnavailable, "upload: the fetch proxy answered %d", resp.StatusCode)
	}
	if br.Buffered() == 0 {
		return conn, nil
	}
	// Bytes the target sent right after the proxy's answer were read into
	// br; they must reach the client before the rest of the stream.
	return &bufferedConn{Conn: conn, r: io.MultiReader(br, conn)}, nil
}

// noDeadline clears a connection's deadline.
var noDeadline = time.Time{}

// bufferedConn is a net.Conn whose reads drain r (bytes already buffered,
// then the connection) first.
type bufferedConn struct {
	net.Conn
	r io.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
