// Package remote executes commands on servers over SSH.
//
// This is the only part of buidl that runs arbitrary privileged commands on
// machines you own, so two properties are treated as non-negotiable:
//
//   - Host keys are verified. Cluster bootstrap installs root-level software and
//     copies a cluster-admin credential back, so an unverified connection is a
//     machine-in-the-middle window at the worst possible moment.
//   - Commands are never assembled by pasting user data into a shell string.
//     Values that reach a remote shell are single-quote escaped, and file content
//     is streamed over stdin rather than interpolated into an echo.
package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Config describes how to reach a server.
type Config struct {
	Host string
	Port int
	User string

	// KeyPath is an explicit private key. When empty, the ssh-agent is tried
	// first, then conventional key locations.
	KeyPath string

	// KnownHosts overrides the host key database path.
	KnownHosts string

	// AcceptNewHostKeys trusts and records keys not already known. See the
	// security note in config.SSH.
	AcceptNewHostKeys bool

	// Sudo prefixes privileged commands with sudo -n.
	Sudo bool

	// Timeout bounds connection establishment.
	Timeout time.Duration
}

// Client is a connection to one server.
type Client struct {
	cfg    Config
	client *ssh.Client
	// addr is the resolved host:port, used in error messages.
	addr string
}

// Result is the outcome of a command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// CommandError reports a command that ran but exited non-zero.
//
// It carries stderr because a remote failure's cause is almost always there, and
// forcing the caller to re-run with logging enabled to see it wastes time.
type CommandError struct {
	Command  string
	Host     string
	ExitCode int
	Stderr   string
}

func (e *CommandError) Error() string {
	msg := fmt.Sprintf("%s: command failed (exit %d): %s", e.Host, e.ExitCode, e.Command)
	if trimmed := strings.TrimSpace(e.Stderr); trimmed != "" {
		msg += "\n  " + strings.ReplaceAll(trimmed, "\n", "\n  ")
	}
	return msg
}

// Dial connects to a server.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Port == 0 {
		cfg.Port = 22
	}
	if cfg.User == "" {
		cfg.User = "root"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}

	auths, err := authMethods(cfg)
	if err != nil {
		return nil, err
	}

	hostKeyCallback, err := hostKeyVerifier(cfg)
	if err != nil {
		return nil, err
	}

	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))

	clientConfig := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            auths,
		HostKeyCallback: hostKeyCallback,
		Timeout:         cfg.Timeout,
	}

	// net.Dialer honors the context, which ssh.Dial does not, so a cancelled
	// command stops waiting on an unreachable host immediately.
	dialer := &net.Dialer{Timeout: cfg.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w\n\nhint: is the host reachable and is port %d open?", addr, err, cfg.Port)
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, clientConfig)
	if err != nil {
		conn.Close()
		return nil, describeHandshakeError(cfg, addr, err)
	}

	return &Client{
		cfg:    cfg,
		client: ssh.NewClient(sshConn, chans, reqs),
		addr:   addr,
	}, nil
}

// describeHandshakeError turns SSH's terse failures into actionable messages.
func describeHandshakeError(cfg Config, addr string, err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unable to authenticate"):
		detail := ""
		if cfg.KeyPath == "" {
			// Naming the count matters: the usual cause is an agent full of keys
			// for other hosts, and "authentication failed" alone does not suggest
			// that pinning one key would fix it.
			if n := countAvailableKeys(); n > 0 {
				detail = fmt.Sprintf("\n\nbuidl offered %d key(s) from ssh-agent and ~/.ssh; the server accepted none.", n)
			}
		}
		return fmt.Errorf("authentication failed for %s@%s%s\n\n"+
			"hint: confirm the key is in the server's authorized_keys, then pin it:\n"+
			"  infra:\n    ssh:\n      user: %s\n      keyPath: ~/.ssh/id_ed25519",
			cfg.User, addr, detail, cfg.User)
	case strings.Contains(msg, "knownhosts: key mismatch"):
		return fmt.Errorf("HOST KEY MISMATCH for %s\n\n"+
			"The server presented a different key than the one recorded in known_hosts.\n"+
			"This can mean the machine was rebuilt — or that traffic is being intercepted.\n\n"+
			"If you rebuilt it, remove the stale entry:\n  ssh-keygen -R %s", addr, cfg.Host)
	case strings.Contains(msg, "knownhosts: key is unknown"):
		return unknownHostKeyError(cfg, addr)
	}
	return fmt.Errorf("ssh handshake with %s failed: %w", addr, err)
}

// unknownHostKeyError explains how to trust a new server.
func unknownHostKeyError(cfg Config, addr string) error {
	return fmt.Errorf("unknown host key for %s\n\n"+
		"buidl verifies host keys because cluster bootstrap installs root-level software\n"+
		"and copies a cluster-admin credential back from this machine.\n\n"+
		"Record the key after checking it against your provider's console:\n"+
		"  ssh-keyscan -p %d %s >> ~/.ssh/known_hosts\n\n"+
		"Or, to trust new keys automatically for this fleet:\n"+
		"  infra:\n    ssh:\n      acceptNewHostKeys: true",
		addr, cfg.Port, cfg.Host)
}

// hostKeyVerifier builds the host key callback.
func hostKeyVerifier(cfg Config) (ssh.HostKeyCallback, error) {
	path := cfg.KnownHosts
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locating home directory for known_hosts: %w", err)
		}
		path = filepath.Join(home, ".ssh", "known_hosts")
	}

	// knownhosts.New fails on a missing file, but a missing file is normal on a
	// fresh machine. Create it so the strict path has something to read, and so
	// AcceptNewHostKeys has somewhere to append.
	if err := ensureFile(path); err != nil {
		return nil, err
	}

	verify, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	if !cfg.AcceptNewHostKeys {
		return verify, nil
	}

	// accept-new semantics: an unknown key is recorded, but a *mismatched* key is
	// still a hard failure. Downgrading a mismatch would defeat the point.
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := verify(hostname, remote, key)
		if err == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) && len(keyErr.Want) == 0 {
			return appendKnownHost(path, hostname, remote, key)
		}
		return err
	}, nil
}

// ensureFile creates an empty file and its parent directory if absent.
func ensureFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	return f.Close()
}

// appendKnownHost records a newly seen host key.
func appendKnownHost(path, hostname string, remote net.Addr, key ssh.PublicKey) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("recording host key in %s: %w", path, err)
	}
	defer f.Close()

	// Record both the name we dialed and the resolved address, matching ssh's
	// behavior, so a later connection by either form validates.
	addresses := []string{knownhosts.Normalize(hostname)}
	if remote != nil {
		if resolved := knownhosts.Normalize(remote.String()); resolved != addresses[0] {
			addresses = append(addresses, resolved)
		}
	}

	line := knownhosts.Line(addresses, key)
	if _, err := fmt.Fprintln(f, line); err != nil {
		return fmt.Errorf("recording host key in %s: %w", path, err)
	}
	return nil
}

// maxOfferedKeys bounds how many public keys are offered in one connection.
//
// sshd's MaxAuthTries defaults to 6 and counts every key offered, so a developer
// with a well-populated agent can be disconnected before the key that would have
// worked is ever tried.
const maxOfferedKeys = 6

// authMethods assembles authentication.
//
// Every available signer is offered through a *single* publickey method rather
// than one method per source. Separate methods do not reliably fall through: an
// agent holding keys the server rejects can end the exchange before an on-disk
// key that would have worked is offered. Since agent keys frequently have nothing
// to do with the server being deployed to, that failure is common and its message
// ("authentication failed") gives no hint that a usable key was sitting unused.
func authMethods(cfg Config) ([]ssh.AuthMethod, error) {
	// An explicit key wins, since it is what the user asked for, and offering
	// only it keeps the attempt count at one.
	if cfg.KeyPath != "" {
		signer, err := loadKey(expandHome(cfg.KeyPath))
		if err != nil {
			return nil, err
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	}

	var signers []ssh.Signer
	seen := map[string]bool{}

	// add records a signer, skipping duplicates. The same key is usually both in
	// the agent and on disk, and offering it twice wastes an attempt.
	add := func(signer ssh.Signer) {
		fingerprint := string(signer.PublicKey().Marshal())
		if seen[fingerprint] {
			return
		}
		seen[fingerprint] = true
		signers = append(signers, signer)
	}

	// The agent goes first: it handles encrypted keys and hardware tokens without
	// buidl ever touching key material or prompting for a passphrase.
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			if agentSigners, err := agent.NewClient(conn).Signers(); err == nil {
				for _, signer := range agentSigners {
					add(signer)
				}
			}
		}
	}

	// Then the conventional locations, strongest algorithm first.
	if home, err := os.UserHomeDir(); err == nil {
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			if signer, err := loadKey(filepath.Join(home, ".ssh", name)); err == nil {
				add(signer)
			}
		}
	}

	if len(signers) == 0 {
		return nil, fmt.Errorf("no SSH credentials found\n\n" +
			"hint: add a key to ssh-agent, or set infra.ssh.keyPath")
	}

	// Beyond the server's limit the extra keys are never reached, so trimming is
	// better than being disconnected mid-exchange.
	if len(signers) > maxOfferedKeys {
		signers = signers[:maxOfferedKeys]
	}

	return []ssh.AuthMethod{ssh.PublicKeys(signers...)}, nil
}

// countAvailableKeys reports how many keys would be offered, for error messages.
func countAvailableKeys() int {
	methods, err := authMethods(Config{})
	if err != nil || len(methods) == 0 {
		return 0
	}
	// The count is not recoverable from the AuthMethod, so recompute cheaply.
	n := 0
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			if signers, err := agent.NewClient(conn).Signers(); err == nil {
				n += len(signers)
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			if _, err := loadKey(filepath.Join(home, ".ssh", name)); err == nil {
				n++
			}
		}
	}
	return n
}

// loadKey reads and parses a private key.
func loadKey(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading SSH key %s: %w", path, err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		if _, isPassphrase := err.(*ssh.PassphraseMissingError); isPassphrase {
			// buidl deliberately does not prompt for or store passphrases; the
			// agent exists for exactly this.
			return nil, fmt.Errorf("SSH key %s is passphrase-protected\n\nhint: add it to your agent with `ssh-add %s`", path, path)
		}
		return nil, fmt.Errorf("parsing SSH key %s: %w", path, err)
	}
	return signer, nil
}

// expandHome resolves a leading ~ in a path.
func expandHome(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~"))
}

// Host returns the address this client is connected to.
func (c *Client) Host() string { return c.cfg.Host }

// Close releases the connection.
func (c *Client) Close() error {
	if c.client == nil {
		return nil
	}
	return c.client.Close()
}

// Run executes a command and returns its output, failing on a non-zero exit.
func (c *Client) Run(ctx context.Context, command string) (*Result, error) {
	result, err := c.run(ctx, command, nil)
	if err != nil {
		return result, err
	}
	if result.ExitCode != 0 {
		return result, &CommandError{
			Command:  command,
			Host:     c.cfg.Host,
			ExitCode: result.ExitCode,
			Stderr:   result.Stderr,
		}
	}
	return result, nil
}

// Try executes a command and reports the exit code without treating non-zero as
// an error. Use for probes such as "is this service active".
func (c *Client) Try(ctx context.Context, command string) (*Result, error) {
	return c.run(ctx, command, nil)
}

// Sudo executes a command with elevated privileges when configured to.
func (c *Client) Sudo(ctx context.Context, command string) (*Result, error) {
	return c.Run(ctx, c.elevate(command))
}

// TrySudo is Sudo without treating a non-zero exit as an error.
func (c *Client) TrySudo(ctx context.Context, command string) (*Result, error) {
	return c.run(ctx, c.elevate(command), nil)
}

// elevate wraps a command in sudo when needed.
func (c *Client) elevate(command string) string {
	if !c.cfg.Sudo || c.cfg.User == "root" {
		return command
	}
	// -n fails immediately rather than hanging on a password prompt that nobody
	// is present to answer.
	return "sudo -n -- sh -c " + Quote(command)
}

// run executes a command, optionally piping stdin.
func (c *Client) run(ctx context.Context, command string, stdin io.Reader) (*Result, error) {
	session, err := c.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("%s: opening session: %w", c.cfg.Host, err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	if stdin != nil {
		session.Stdin = stdin
	}

	// Cancellation must actually interrupt a running command, not just abandon
	// the goroutine waiting on it.
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()

	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGTERM)
		_ = session.Close()
		return nil, ctx.Err()
	case runErr := <-done:
		result := &Result{
			Stdout: stdout.String(),
			Stderr: stderr.String(),
		}
		if runErr == nil {
			return result, nil
		}
		var exitErr *ssh.ExitError
		if errors.As(runErr, &exitErr) {
			result.ExitCode = exitErr.ExitStatus()
			return result, nil
		}
		return result, fmt.Errorf("%s: running %q: %w", c.cfg.Host, command, runErr)
	}
}

// WriteFile writes content to a remote path with the given mode.
//
// Content is streamed over stdin rather than interpolated into a shell command,
// so it can contain any bytes — newlines, quotes, YAML — without escaping
// concerns, and never appears in the process list or shell history.
func (c *Client) WriteFile(ctx context.Context, path, content string, mode string) error {
	dir := filepath.Dir(path)
	command := fmt.Sprintf("mkdir -p %s && cat > %s && chmod %s %s",
		Quote(dir), Quote(path), Quote(mode), Quote(path))

	result, err := c.run(ctx, c.elevate(command), strings.NewReader(content))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return &CommandError{
			Command:  "write " + path,
			Host:     c.cfg.Host,
			ExitCode: result.ExitCode,
			Stderr:   result.Stderr,
		}
	}
	return nil
}

// ReadFile reads a remote file.
func (c *Client) ReadFile(ctx context.Context, path string) (string, error) {
	result, err := c.Sudo(ctx, "cat "+Quote(path))
	if err != nil {
		return "", err
	}
	return result.Stdout, nil
}

// Exists reports whether a remote path exists.
func (c *Client) Exists(ctx context.Context, path string) (bool, error) {
	result, err := c.TrySudo(ctx, "test -e "+Quote(path))
	if err != nil {
		return false, err
	}
	return result.ExitCode == 0, nil
}

// Quote renders s as a single shell argument, safe against injection.
//
// POSIX single quotes suppress all interpretation; the only character needing
// care is the single quote itself, closed and reopened around an escaped one.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	// Fast path: nothing that a shell would interpret.
	if !strings.ContainsAny(s, "\\\"' \t\n$&|;<>()*?[]#~=%!{}`") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
