package vm

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/giantswarm/vm-manager/internal/apierr"
)

const (
	sshUser        = "root"
	sshPort        = "22"
	sshTimeout     = 15 * time.Second
	hostKeyFile    = "ssh_host_key"
	forwardHost    = "127.0.0.1:0"
	defaultConsole = 100
	maxConsole     = 10000
)

// Exec runs cmd on the guest as root over ssh through the virtual network,
// authenticated with the key vm-manager generated for the VM. The guest's
// host key is pinned on first use. A non-zero exit is a result, not an error.
func (s *Service) Exec(ctx context.Context, id string, cmd []string) (ExecResult, error) {
	if len(cmd) == 0 {
		return ExecResult{}, fmt.Errorf("%w: command is empty", apierr.ErrInvalid)
	}
	e, err := s.entry(id)
	if err != nil {
		return ExecResult{}, err
	}
	s.mu.Lock()
	rec := e.rec.clone()
	live := e.proc != nil
	s.mu.Unlock()
	if !live {
		return ExecResult{}, fmt.Errorf("%w: vm %s is not running (%s)", apierr.ErrConflict, id, rec.State)
	}
	nw, err := s.opts.Networks.Get(rec.Network)
	if err != nil {
		return ExecResult{}, err
	}
	signer, err := loadSigner(rec.Paths.SSHKey)
	if err != nil {
		return ExecResult{}, err
	}

	addr := net.JoinHostPort(rec.IP, sshPort)
	conn, err := nw.Dial(ctx, addr)
	if err != nil {
		return ExecResult{}, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	cfg := &ssh.ClientConfig{
		User:            sshUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: s.pinHostKey(e),
		Timeout:         sshTimeout,
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		return ExecResult{}, fmt.Errorf("ssh %s: %w", addr, err)
	}
	client := ssh.NewClient(c, chans, reqs)
	defer func() { _ = client.Close() }()
	sess, err := client.NewSession()
	if err != nil {
		return ExecResult{}, fmt.Errorf("ssh session: %w", err)
	}
	defer func() { _ = sess.Close() }()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	err = sess.Run(shellJoin(cmd))
	res := ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *ssh.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitStatus()
	default:
		return res, fmt.Errorf("ssh run: %w", err)
	}
	return res, nil
}

// pinHostKey trusts the guest's host key on first use and rejects a
// different one afterwards.
func (s *Service) pinHostKey(e *entry) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		e.sshMu.Lock()
		defer e.sshMu.Unlock()
		path := filepath.Join(e.rec.Paths.Dir, hostKeyFile)
		want := ssh.MarshalAuthorizedKey(key)
		have, err := os.ReadFile(path) // #nosec G304 -- state dir file.
		switch {
		case errors.Is(err, os.ErrNotExist):
			return writeAtomic(path, want, 0o600)
		case err != nil:
			return err
		case !bytes.Equal(have, want):
			return fmt.Errorf("host key of vm %s changed", e.rec.ID)
		}
		return nil
	}
}

// generateSSHKey writes a fresh ed25519 private key to path and returns the
// authorized_keys line of its public half.
func generateSSHKey(path, id string) (string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate ssh key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "vm-manager@"+id)
	if err != nil {
		return "", fmt.Errorf("encode ssh key: %w", err)
	}
	if err := writeAtomic(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", fmt.Errorf("write ssh key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	return line + " vm-manager@" + id, nil
}

func loadSigner(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- state dir file.
	if err != nil {
		return nil, fmt.Errorf("read ssh key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse ssh key: %w", err)
	}
	return signer, nil
}

// shellJoin quotes cmd for the guest's shell: each word in single quotes.
func shellJoin(cmd []string) string {
	words := make([]string, len(cmd))
	for i, w := range cmd {
		words[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
	}
	return strings.Join(words, " ")
}

// Console returns the last lines of the serial console (100 when lines is
// not positive, at most 10000).
func (s *Service) Console(id string, lines int) (string, error) {
	v, err := s.Get(id)
	if err != nil {
		return "", err
	}
	if lines <= 0 {
		lines = defaultConsole
	}
	if lines > maxConsole {
		lines = maxConsole
	}
	out, err := tailLines(v.Paths.Console, lines)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return out, err
}

// Forward exposes a guest TCP port on a loopback address of the host and
// returns that address. The forward lives until the VM is deleted; it does
// not need the VM to be running.
func (s *Service) Forward(ctx context.Context, id string, port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("%w: port %d out of range", apierr.ErrInvalid, port)
	}
	e, err := s.entry(id)
	if err != nil {
		return "", err
	}
	rec := s.snapshot(e)
	if rec.State == StateDeleting || rec.IP == "" {
		return "", fmt.Errorf("%w: vm %s has no address (%s)", apierr.ErrConflict, id, rec.State)
	}
	nw, err := s.opts.Networks.Get(rec.Network)
	if err != nil {
		return "", err
	}
	fwd, err := nw.Forward(ctx, forwardHost, rec.IP, port)
	if err != nil {
		return "", fmt.Errorf("forward port %d: %w", port, err)
	}
	s.mu.Lock()
	e.forwards = append(e.forwards, fwd)
	s.mu.Unlock()
	return fwd.Addr().String(), nil
}
