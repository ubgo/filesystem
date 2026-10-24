package sftp_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/ubgo/filesystem"
	fssftp "github.com/ubgo/filesystem/adapters/sftp"
)

// clientKey is a fresh ed25519 key pair for a test SSH client.
func clientKey(t *testing.T) (ed25519.PrivateKey, ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return priv, signer.PublicKey()
}

func sftpMux() *filesystem.Mux {
	mux := filesystem.NewMux()
	_ = mux.Register(fssftp.Scheme, fssftp.Opener)
	return mux
}

// TestOpenerIdentityPassphrase proves an encrypted identity opens with its
// passphrase from the environment, and is refused without it, with a wrong
// one, or with a passphrase but no identity.
func TestOpenerIdentityPassphrase(t *testing.T) {
	ctx := t.Context()
	priv, pub := clientKey(t)
	addr, _ := sshServer(t, pub, true)
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(t.TempDir(), "id")
	if err := os.WriteFile(identity, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KEY_PASS", "hunter2")
	t.Setenv("KEY_BAD_PASS", "nope")
	base := "sftp://alice@" + addr + t.TempDir() + "?insecure_ignore_host_key=true&identity=" + url.QueryEscape(identity)
	d, err := sftpMux().Open(ctx, "sftp", base+"&identity_passphrase_env=KEY_PASS")
	if err != nil {
		t.Fatalf("open with the passphrase = %v", err)
	}
	if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Errorf("write = %v", err)
	}
	_ = d.Close()
	for _, dsn := range []string{
		base,
		base + "&identity_passphrase_env=KEY_BAD_PASS",
		"sftp://alice@" + addr + "/tmp?insecure_ignore_host_key=true&identity_passphrase_env=KEY_PASS",
	} {
		_, err := sftpMux().Open(ctx, "sftp", dsn)
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
}

// serveAgent runs an ssh-agent holding keys on a unix socket and returns
// the socket's path. The socket lives in a short directory, since a unix
// socket path is limited to about 100 bytes.
func serveAgent(t *testing.T, keys ...ed25519.PrivateKey) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ring := agent.NewKeyring()
	for _, k := range keys {
		if err := ring.Add(agent.AddedKey{PrivateKey: k}); err != nil {
			t.Fatal(err)
		}
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = agent.ServeAgent(ring, c); _ = c.Close() }()
		}
	}()
	return sock
}

// TestOpenerAgent proves agent=true authenticates with the agent's keys,
// that an agent without the right key is refused by the server, and that
// agent=true without SSH_AUTH_SOCK, or with a non-boolean value, is
// refused at open.
func TestOpenerAgent(t *testing.T) {
	ctx := t.Context()
	priv, pub := clientKey(t)
	other, _ := clientKey(t)
	addr, _ := sshServer(t, pub, true)
	dsn := "sftp://alice@" + addr + t.TempDir() + "?insecure_ignore_host_key=true&agent=true"

	t.Setenv("SSH_AUTH_SOCK", serveAgent(t, priv))
	d, err := sftpMux().Open(ctx, "sftp", dsn)
	if err != nil {
		t.Fatalf("open through the agent = %v", err)
	}
	if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Errorf("write = %v", err)
	}
	_ = d.Close()

	t.Setenv("SSH_AUTH_SOCK", serveAgent(t, other))
	_, err = sftpMux().Open(ctx, "sftp", dsn)
	wantCode(t, err, filesystem.CodePermission)

	t.Setenv("SSH_AUTH_SOCK", filepath.Join(t.TempDir(), "gone"))
	_, err = sftpMux().Open(ctx, "sftp", dsn)
	wantCode(t, err, filesystem.CodeUnavailable)

	t.Setenv("SSH_AUTH_SOCK", "")
	_, err = sftpMux().Open(ctx, "sftp", dsn)
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = sftpMux().Open(ctx, "sftp", "sftp://alice@"+addr+"/tmp?insecure_ignore_host_key=true&agent=maybe")
	wantCode(t, err, filesystem.CodeInvalidArgument)
}
