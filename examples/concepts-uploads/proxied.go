package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/upload"
)

// importerAgent is the User-Agent the importer announces itself with.
const importerAgent = "acme-importer/1.0 (+https://acme.example/bot)"

// connectProxy is a minimal CONNECT proxy, standing in for Smokescreen or
// Squid. It records the target of every CONNECT it is asked for.
type connectProxy struct {
	mu      sync.Mutex
	targets []string
}

func (p *connectProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
		return
	}
	p.mu.Lock()
	p.targets = append(p.targets, r.Host)
	p.mu.Unlock()
	upstream, err := net.Dial("tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	_, _ = io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n")
	go func() { _, _ = io.Copy(upstream, buf); _ = upstream.Close() }()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
}

// asked returns the CONNECT targets so far.
func (p *connectProxy) asked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}

// throughProxy imports a URL named by host name through an egress proxy,
// with the importer's own User-Agent. The fetch resolves the name and
// checks the address itself, then asks the proxy for that address, so
// the proxy never resolves anything.
// ds:def id=throughproxy-n9q4v7hs
func throughProxy(ctx context.Context, out io.Writer) error {
	var agent string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent = r.Header.Get("User-Agent")
		_, _ = io.WriteString(w, pdfFile)
	}))
	defer remote.Close()
	pp := &connectProxy{}
	proxySrv := httptest.NewServer(pp)
	defer proxySrv.Close()
	proxy, err := url.Parse(proxySrv.URL)
	if err != nil {
		return err
	}
	disk, err := newDisk()
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(strings.TrimPrefix(remote.URL, "http://"))
	if err != nil {
		return err
	}
	link := "http://localhost:" + port + "/files/q3.pdf" // a name, not an address
	res, err := upload.FromURL(ctx, disk, link, policy, upload.FetchOptions{
		Header:    http.Header{"User-Agent": {importerAgent}},
		Proxy:     proxy,
		AllowAddr: func(a netip.Addr) bool { return a.IsLoopback() }, // the demo's server is local
	})
	if err != nil {
		return err
	}
	targets := pp.asked()
	addresses := len(targets) > 0
	for _, t := range targets { // one per address tried: ::1, then 127.0.0.1
		host, _, err := net.SplitHostPort(t)
		addresses = addresses && err == nil && net.ParseIP(host) != nil
	}
	fmt.Fprintf(out, "through the proxy: %q %s; proxy asked for addresses, never the name: %t; User-Agent sent: %t\n",
		res.Filename, res.ContentType, addresses, agent == importerAgent)
	_, err = upload.FromURL(ctx, disk, "http://169.254.169.254/latest/meta-data/", policy, upload.FetchOptions{Proxy: proxy})
	fmt.Fprintf(out, "metadata address through the proxy: %s, proxy asked: %t\n", filesystem.CodeOf(err), len(pp.asked()) > len(targets))
	return nil
}
