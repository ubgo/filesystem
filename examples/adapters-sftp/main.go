// Command adapters-sftp walks through the SFTP driver: opening a disk from
// a DSN with a known_hosts file and a private key, opening one from a
// Config over an SSH connection the application dialled itself, atomic
// and conditional writes, visibility as file modes, and what SFTP cannot
// store.
//
// It starts an in-process SSH server with the SFTP subsystem on a loopback
// port (serving a temporary directory), so it needs no remote host.
//
//	go run ./adapters-sftp            (from the examples directory)
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/ubgo/filesystem"
	fssftp "github.com/ubgo/filesystem/adapters/sftp"
)

// sftpUser is the account the example logs in as.
const sftpUser = "partner"

// subsystemSFTP is the SSH subsystem name SFTP runs under.
const subsystemSFTP = "sftp"

// credentials is what a deployment keeps outside the DSN: the server's
// host key line in known_hosts, and the client's private key file.
type credentials struct {
	KnownHosts string // path of a known_hosts file naming the server
	Identity   string // path of an OpenSSH private key file
	clientKey  ssh.Signer
	hostKey    ssh.Signer
}

func main() {
	dir, err := os.MkdirTemp("", "adapters-sftp-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is main without the process: dir holds the keys and the served
// directory, and every step prints what it saw to out.
func run(ctx context.Context, out io.Writer, dir string) error {
	creds, err := newCredentials(dir)
	if err != nil {
		return err
	}
	addr, stop, err := startServer(creds)
	if err != nil {
		return err
	}
	defer stop()
	if err := writeKnownHosts(creds, addr); err != nil {
		return err
	}
	root := filepath.Join(dir, "exchange")
	disk, err := openWithDSN(ctx, addr, root, creds)
	if err != nil {
		return err
	}
	defer func() { _ = disk.Close() }()
	fmt.Fprintf(out, "capabilities: %s\n", disk.Capabilities())
	for _, step := range []func(context.Context, io.Writer, *filesystem.Disk) error{dropAndPickUp, refusedOptions} {
		if err := step(ctx, out, disk); err != nil {
			return err
		}
	}
	return openWithConfig(ctx, out, addr, root, creds)
}

// sftpDSN is the DSN of root on the server at addr. The path is absolute
// on the server; no secret is in it, only the paths of the files that hold
// them.
// ds:def id=sftpdsn-3chvpnn6
func sftpDSN(addr, root string, creds credentials) string {
	q := url.Values{}
	q.Set(fssftp.ParamKnownHosts, creds.KnownHosts)
	q.Set(fssftp.ParamIdentity, creds.Identity)
	return fssftp.Scheme + "://" + sftpUser + "@" + addr + root + "?" + q.Encode()
}

// openWithDSN dials the server, verifies its host key against known_hosts,
// logs in with the private key and opens root as a disk.
// ds:def id=openwithdsn-n7y5ygk9
func openWithDSN(ctx context.Context, addr, root string, creds credentials) (*filesystem.Disk, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(fssftp.Scheme, fssftp.Opener); err != nil {
		return nil, err
	}
	return mux.Open(ctx, "partner", sftpDSN(addr, root, creds))
}

// dropAndPickUp is the partner-exchange flow: write a file atomically,
// refuse to overwrite one that is already there, list, and move the file
// into an archive with an atomic rename.
// ds:def id=dropandpickup-hrcthtrj
func dropAndPickUp(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	report := []byte("sku,qty\nA-1,4\n")
	if _, err := disk.WriteBytes(ctx, "outbound/orders.csv", report, filesystem.WriteOptions{IfNotExists: true}); err != nil {
		return err
	}
	_, err := disk.WriteBytes(ctx, "outbound/orders.csv", report, filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "second conditional write: %s\n", filesystem.CodeOf(err))
	e, err := disk.Stat(ctx, "outbound/orders.csv")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stat: %s %d bytes %s (type from extension: %s)\n", e.Key, e.Size, e.Visibility, e.ContentType)
	if _, err := disk.Move(ctx, "outbound/orders.csv", "archive/2026-10/orders.csv"); err != nil {
		return err
	}
	for entry, err := range disk.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "listed: %s\n", entry.Key)
	}
	return nil
}

// refusedOptions shows what SFTP cannot store: metadata and HTTP headers
// are CodeUnsupported instead of being dropped.
// ds:def id=refusedoptions-kha3m599
func refusedOptions(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	_, err := disk.WriteBytes(ctx, "x.txt", []byte("x"), filesystem.WriteOptions{Metadata: map[string]string{"k": "v"}})
	fmt.Fprintf(out, "metadata: %s\n", filesystem.CodeOf(err))
	_, err = disk.WriteBytes(ctx, "x.txt", []byte("x"), filesystem.WriteOptions{Headers: filesystem.HTTPHeaders{CacheControl: "no-store"}})
	fmt.Fprintf(out, "headers: %s\n", filesystem.CodeOf(err))
	return nil
}

// openWithConfig is the code path for an application that manages its
// own SSH connection (a jump host, an agent, keep-alives): dial, start an
// SFTP client, and hand it to New with a Closer so Disk.Close closes it.
// ds:def id=openwithconfig-k4zbz2j8
func openWithConfig(ctx context.Context, out io.Writer, addr, root string, creds credentials) error {
	hostKeys, err := knownhosts.New(creds.KnownHosts)
	if err != nil {
		return err
	}
	conn, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            sftpUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(creds.clientKey)},
		HostKeyCallback: hostKeys,
	})
	if err != nil {
		return err
	}
	client, err := sftp.NewClient(conn)
	if err != nil {
		return errors.Join(err, conn.Close())
	}
	drv, err := fssftp.New(fssftp.Config{Client: client, Root: root, Closer: conn})
	if err != nil {
		return errors.Join(err, client.Close(), conn.Close())
	}
	disk, err := filesystem.NewDisk("partner-config", drv)
	if err != nil {
		return err
	}
	defer func() { _ = disk.Close() }()
	ok, err := disk.Exists(ctx, "archive/2026-10/orders.csv")
	fmt.Fprintf(out, "via Config: archived file exists=%t\n", ok)
	return err
}

// newCredentials generates a host key and a client key, and writes the
// client key where the DSN's identity parameter points.
func newCredentials(dir string) (credentials, error) {
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return credentials{}, err
	}
	_, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return credentials{}, err
	}
	c := credentials{KnownHosts: filepath.Join(dir, "known_hosts"), Identity: filepath.Join(dir, "id_ed25519")}
	if c.hostKey, err = ssh.NewSignerFromKey(hostPriv); err != nil {
		return credentials{}, err
	}
	if c.clientKey, err = ssh.NewSignerFromKey(clientPriv); err != nil {
		return credentials{}, err
	}
	block, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		return credentials{}, err
	}
	return c, os.WriteFile(c.Identity, pem.EncodeToMemory(block), 0o600)
}

// writeKnownHosts records the server's host key for its address, the line
// ssh-keyscan would print.
// ds:def id=writeknownhosts-jg44gy3b
func writeKnownHosts(c credentials, addr string) error {
	line := knownhosts.Line([]string{addr}, c.hostKey.PublicKey())
	return os.WriteFile(c.KnownHosts, []byte(line+"\n"), 0o600)
}

// startServer runs an SSH server on a loopback port that accepts only the
// client key and serves SFTP. stop closes the listener.
func startServer(c credentials) (addr string, stop func(), err error) {
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if string(k.Marshal()) == string(c.clientKey.PublicKey().Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(c.hostKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveConn(conn, cfg)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }, nil
}

// serveConn completes the SSH handshake and serves the SFTP subsystem on
// every session channel.
func serveConn(conn net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, requests, err := nc.Accept()
		if err != nil {
			continue
		}
		go serveSession(ch, requests)
	}
}

// serveSession answers a session's requests, starting pkg/sftp's server
// when the client asks for the sftp subsystem.
func serveSession(ch ssh.Channel, requests <-chan *ssh.Request) {
	for req := range requests {
		ok := req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == subsystemSFTP
		_ = req.Reply(ok, nil)
		if ok {
			if srv, err := sftp.NewServer(ch); err == nil {
				_ = srv.Serve()
			}
			_ = ch.Close()
		}
	}
}
