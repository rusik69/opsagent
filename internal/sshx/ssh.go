package sshx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Options configures the SSH Runner.
type Options struct {
	// UseAgent enables ssh-agent authentication.
	UseAgent bool
	// PrivateKeyPath is an optional PEM private key used for authentication.
	PrivateKeyPath string
	// KnownHosts is a path to a known_hosts file, or "insecure" to skip
	// host key verification (development only).
	KnownHosts string
	// ConnectTimeout bounds SSH handshake.
	ConnectTimeout time.Duration
	// CommandTimeout bounds a single command execution.
	CommandTimeout time.Duration
	// Resolver maps host names to SSH targets.
	Resolver HostResolver
}

// SSHClient is the real Runner implementation over golang.org/x/crypto/ssh.
// It caches one connection per host and reconnects transparently on failure.
type SSHClient struct {
	opts   Options
	mu     sync.Mutex
	conns  map[string]*ssh.Client
	config *ssh.ClientConfig
}

func NewSSHClient(opts Options) (*SSHClient, error) {
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = 10 * time.Second
	}
	if opts.CommandTimeout <= 0 {
		opts.CommandTimeout = 30 * time.Second
	}
	if opts.Resolver == nil {
		return nil, errors.New("ssh: no host resolver configured")
	}
	auths := []ssh.AuthMethod{}
	if opts.UseAgent {
		if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
			if ag, err := net.Dial("unix", sock); err == nil {
				auths = append(auths, ssh.PublicKeysCallback(agent.NewClient(ag).Signers))
			}
		}
	}
	if opts.PrivateKeyPath != "" {
		key, err := os.ReadFile(opts.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("ssh: read private key: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("ssh: parse private key: %w", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if len(auths) == 0 {
		return nil, errors.New("ssh: no authentication methods configured (agent or private key)")
	}

	hostKeyCB := ssh.InsecureIgnoreHostKey()
	if strings.ToLower(opts.KnownHosts) != "insecure" && opts.KnownHosts != "" {
		cb, err := knownhosts.New(opts.KnownHosts)
		if err != nil {
			return nil, fmt.Errorf("ssh: known_hosts: %w", err)
		}
		hostKeyCB = cb
	}

	cfg := &ssh.ClientConfig{
		User:            "ops",
		Auth:            auths,
		HostKeyCallback: hostKeyCB,
		Timeout:         opts.ConnectTimeout,
	}
	return &SSHClient{opts: opts, conns: map[string]*ssh.Client{}, config: cfg}, nil
}

func (c *SSHClient) resolve(host string) (HostTarget, error) {
	t, ok := c.opts.Resolver.Resolve(host)
	if !ok {
		return HostTarget{}, fmt.Errorf("host %q is not configured", host)
	}
	return t, nil
}

func (c *SSHClient) client(ctx context.Context, host string) (*ssh.Client, error) {
	t, err := c.resolve(host)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	conn, ok := c.conns[host]
	c.mu.Unlock()
	if ok {
		return conn, nil
	}
	addr := net.JoinHostPort(t.Address, fmt.Sprintf("%d", t.Port))
	cfg := *c.config
	cfg.User = t.User
	conn, err = ssh.Dial("tcp", addr, &cfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	c.mu.Lock()
	c.conns[host] = conn
	c.mu.Unlock()
	return conn, nil
}

// drop removes a cached connection (e.g. after it was found stale).
func (c *SSHClient) drop(host string) {
	c.mu.Lock()
	if conn, ok := c.conns[host]; ok {
		conn.Close()
		delete(c.conns, host)
	}
	c.mu.Unlock()
}

// Run executes command on host without a PTY, capturing stdout and stderr.
// On a transport-level failure it drops the cached connection and retries the
// command once against a fresh dial. Legitimate non-zero exits are NOT retried.
func (c *SSHClient) Run(ctx context.Context, host, command string) (Result, error) {
	result, err := c.runOnce(ctx, host, command)
	if err == nil {
		return result, nil
	}
	if _, isExit := err.(*ssh.ExitError); isExit {
		return result, err
	}
	c.drop(host)
	result2, err2 := c.runOnce(ctx, host, command)
	if err2 == nil {
		return result2, nil
	}
	if _, isExit := err2.(*ssh.ExitError); isExit {
		return result2, err2
	}
	return result, err
}

func (c *SSHClient) runOnce(ctx context.Context, host, command string) (Result, error) {
	conn, err := c.client(ctx, host)
	if err != nil {
		return Result{}, err
	}
	sess, err := conn.NewSession()
	if err != nil {
		return Result{}, fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	runCtx, cancel := context.WithTimeout(ctx, c.opts.CommandTimeout)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- sess.Run(command) }()
	start := time.Now()
	select {
	case err = <-errCh:
	case <-runCtx.Done():
		sess.Close()
		err = runCtx.Err()
	}
	dur := time.Since(start)
	result := Result{Stdout: stdout.String(), Stderr: stderr.String(), DurationMS: dur.Milliseconds()}
	if err != nil && result.Stdout == "" {
		if ee, ok := err.(*ssh.ExitError); ok {
			return result, fmt.Errorf("command exited with %d", ee.ExitStatus())
		}
		return result, err
	}
	return result, nil
}

func (c *SSHClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, conn := range c.conns {
		conn.Close()
	}
	c.conns = map[string]*ssh.Client{}
	return nil
}
