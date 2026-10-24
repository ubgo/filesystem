package sftp

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/ubgo/filesystem"
)

// Scheme is the DSN scheme: sftp://user@host:port/absolute/root.
const Scheme = "sftp"

// DSN parameters understood by Opener, besides the Mux's own. Secrets never
// appear in the DSN: a password is read from the environment variable
// named by password_env, a key from the file named by identity.
const (
	// ParamKnownHosts is the known_hosts file that verifies the server.
	// Required unless ParamInsecure is set.
	ParamKnownHosts = "known_hosts"
	// ParamInsecure ("true") skips host key verification. Only for tests
	// and throwaway environments: it allows a man in the middle.
	ParamInsecure = "insecure_ignore_host_key"
	// ParamIdentity is a private key file for public-key authentication.
	ParamIdentity = "identity"
	// ParamPasswordEnv names an environment variable holding the password.
	ParamPasswordEnv = "password_env"
	// ParamIdentityPassphraseEnv names an environment variable holding the
	// passphrase of the ParamIdentity key, for encrypted keys. Without it
	// an encrypted key is refused as CodeInvalidArgument.
	ParamIdentityPassphraseEnv = "identity_passphrase_env"
	// ParamAgent ("true") authenticates with the keys of the running
	// ssh-agent, reached through the socket SSH_AUTH_SOCK names, so keys
	// held by the agent (hardware keys, certificates the agent loaded)
	// never touch the DSN or the disk. The socket is dialled for each
	// connection's handshake and closed after it.
	ParamAgent = "agent"
)

// envAgentSocket is the environment variable OpenSSH clients read the
// agent's socket from; ParamAgent reads the same one.
const envAgentSocket = "SSH_AUTH_SOCK"

// defaultPort is SSH's port.
const defaultPort = "22"

// DefaultDialTimeout bounds one dial (TCP, SSH handshake, SFTP start) when
// the caller's context has no deadline of its own, so a server that accepts
// the connection and then never answers (sshd at MaxStartups) cannot hold a
// reconnect forever. It is the opener's ssh.ClientConfig.Timeout.
const DefaultDialTimeout = 30 * time.Second

// Opener dials the server named by the DSN and opens the root directory in
// its path. The driver keeps the dialer, so a dropped connection is
// redialled with the same host, credentials and host key check.
func Opener(ctx context.Context, u *url.URL) (filesystem.Driver, error) {
	if err := filesystem.RejectParams(u, ParamKnownHosts, ParamInsecure, ParamIdentity, ParamPasswordEnv, ParamIdentityPassphraseEnv, ParamAgent); err != nil {
		return nil, err
	}
	cfg, agentSock, err := clientConfig(u)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), defaultPort)
	}
	dial := sshDialer(host, cfg, agentSock)
	client, closer, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	d, err := New(Config{Client: client, Root: u.Path, Closer: closer, Dial: dial})
	if err != nil {
		_ = closer.Close()
		return nil, err
	}
	return d, nil
}

// sshDialer returns the Dialer for host: TCP, then the SSH handshake, then
// the SFTP subsystem, all bounded by ctx. Neither the handshake nor the
// SFTP start takes a context, so the socket is closed when ctx ends
// (deadline or cancellation), which fails whichever step is running; a ctx
// without a deadline gets cfg.Timeout. A failed TCP dial, an ended ctx or
// a server without SFTP is CodeUnavailable; a failed handshake
// (credentials, host key) is CodePermission. The closer it returns closes
// the client and the SSH connection. With agentSock set (ParamAgent) each
// dial also connects to the ssh-agent there for the handshake; an
// unreachable agent is CodeUnavailable.
func sshDialer(host string, base *ssh.ClientConfig, agentSock string) Dialer {
	return func(ctx context.Context) (*sftp.Client, io.Closer, error) {
		if _, ok := ctx.Deadline(); !ok && base.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, base.Timeout)
			defer cancel()
		}
		var dialer net.Dialer
		cfg := base
		if agentSock != "" {
			// The agent signs only during the handshake, so its connection
			// lives exactly as long as this dial.
			ac, err := dialer.DialContext(ctx, "unix", agentSock)
			if err != nil {
				return nil, nil, filesystem.NewError(filesystem.CodeUnavailable, err)
			}
			defer func() { _ = ac.Close() }()
			withAgent := *base
			withAgent.Auth = append(slices.Clone(base.Auth), ssh.PublicKeysCallback(agent.NewClient(ac).Signers))
			cfg = &withAgent
		}
		raw, err := dialer.DialContext(ctx, "tcp", host)
		if err != nil {
			return nil, nil, filesystem.NewError(filesystem.CodeUnavailable, err)
		}
		stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
		conn, chans, reqs, err := ssh.NewClientConn(raw, host, cfg)
		if err != nil {
			stop()
			_ = raw.Close()
			if ctx.Err() != nil {
				return nil, nil, filesystem.NewError(filesystem.CodeUnavailable, errors.Join(ctx.Err(), err))
			}
			return nil, nil, filesystem.NewError(filesystem.CodePermission, err)
		}
		sshClient := ssh.NewClient(conn, chans, reqs)
		client, err := sftp.NewClient(sshClient)
		// stop reports false when ctx already ended and closed the socket,
		// even if the SFTP start happened to finish first: closing the SSH
		// connection then also ends any client it started.
		if !stop() {
			_ = sshClient.Close()
			return nil, nil, filesystem.NewError(filesystem.CodeUnavailable, errors.Join(ctx.Err(), err))
		}
		if err != nil {
			_ = sshClient.Close()
			return nil, nil, filesystem.NewError(filesystem.CodeUnavailable, err)
		}
		return client, closers{client, sshClient}, nil
	}
}

// clientConfig builds the SSH configuration: user, authentication methods
// and host key verification from the DSN, and returns the ssh-agent socket
// when ParamAgent is set (agent keys are added per dial, see sshDialer).
func clientConfig(u *url.URL) (*ssh.ClientConfig, string, error) {
	q := u.Query()
	if u.Host == "" || u.User == nil || u.User.Username() == "" {
		return nil, "", filesystem.Errorf(filesystem.CodeInvalidArgument, "sftp DSN needs user@host")
	}
	if _, has := u.User.Password(); has {
		return nil, "", filesystem.Errorf(filesystem.CodeInvalidArgument, "sftp DSN must not contain a password; use %s", ParamPasswordEnv)
	}
	cfg := &ssh.ClientConfig{User: u.User.Username(), Timeout: DefaultDialTimeout}
	insecure, err := boolParam(q, ParamInsecure)
	if err != nil {
		return nil, "", err
	}
	if insecure {
		cfg.HostKeyCallback = ssh.InsecureIgnoreHostKey() // explicitly requested; see ParamInsecure
	} else {
		file := q.Get(ParamKnownHosts)
		if file == "" {
			return nil, "", filesystem.Errorf(filesystem.CodeInvalidArgument, "sftp DSN needs %s (or %s=true for tests)", ParamKnownHosts, ParamInsecure)
		}
		cb, err := knownhosts.New(file)
		if err != nil {
			return nil, "", filesystem.NewError(filesystem.CodeInvalidArgument, err)
		}
		cfg.HostKeyCallback = cb
	}
	if file := q.Get(ParamIdentity); file != "" {
		signer, err := identitySigner(file, q.Get(ParamIdentityPassphraseEnv))
		if err != nil {
			return nil, "", err
		}
		cfg.Auth = append(cfg.Auth, ssh.PublicKeys(signer))
	} else if q.Has(ParamIdentityPassphraseEnv) {
		return nil, "", filesystem.Errorf(filesystem.CodeInvalidArgument, "sftp DSN: %s needs %s", ParamIdentityPassphraseEnv, ParamIdentity)
	}
	if env := q.Get(ParamPasswordEnv); env != "" {
		cfg.Auth = append(cfg.Auth, ssh.Password(os.Getenv(env)))
	}
	useAgent, err := boolParam(q, ParamAgent)
	if err != nil {
		return nil, "", err
	}
	var sock string
	if useAgent {
		if sock = os.Getenv(envAgentSocket); sock == "" {
			return nil, "", filesystem.Errorf(filesystem.CodeInvalidArgument, "sftp DSN: %s=true but %s is not set", ParamAgent, envAgentSocket)
		}
	}
	if len(cfg.Auth) == 0 && sock == "" {
		return nil, "", filesystem.Errorf(filesystem.CodeInvalidArgument, "sftp DSN needs %s, %s or %s=true", ParamIdentity, ParamPasswordEnv, ParamAgent)
	}
	return cfg, sock, nil
}

// boolParam reads an optional boolean DSN parameter; absent is false.
func boolParam(q url.Values, name string) (bool, error) {
	raw := q.Get(name)
	if raw == "" {
		return false, nil
	}
	on, err := strconv.ParseBool(raw)
	if err != nil {
		return false, filesystem.Errorf(filesystem.CodeInvalidArgument, "%s=%q is not a boolean", name, raw)
	}
	return on, nil
}

// identitySigner reads the private key file, decrypting it with the
// passphrase in the environment variable passEnv when one is named. An
// encrypted key without a passphrase, or a wrong one, is
// CodeInvalidArgument, like an unreadable file.
func identitySigner(file, passEnv string) (ssh.Signer, error) {
	pem, err := os.ReadFile(file)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	var signer ssh.Signer
	if passEnv != "" {
		signer, err = ssh.ParsePrivateKeyWithPassphrase(pem, []byte(os.Getenv(passEnv)))
	} else {
		signer, err = ssh.ParsePrivateKey(pem)
	}
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	return signer, nil
}

// closers closes the SFTP client, then the SSH connection under it.
type closers struct {
	client *sftp.Client
	ssh    *ssh.Client
}

func (c closers) Close() error {
	return errors.Join(c.client.Close(), c.ssh.Close())
}
