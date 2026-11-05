// Package ssh runs commands and uploads files over SSH.
package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"

	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

// Config describes how to reach the VM.
type Config struct {
	Host   string
	Port   int
	User   string
	Signer gossh.Signer
}

// Comm is an open SSH connection.
type Comm struct {
	client *gossh.Client
}

// ExitError is returned by Run when the remote command exits non-zero.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string { return fmt.Sprintf("exit code %d", e.Code) }

// Dial opens one SSH connection.
func Dial(ctx context.Context, cfg Config) (*Comm, error) {
	sshCfg := &gossh.ClientConfig{
		User: cfg.User,
		Auth: []gossh.AuthMethod{gossh.PublicKeys(cfg.Signer)},
		// The VM is created by the build itself and its host key is
		// generated at first boot, so there is nothing to check it against.
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	conn, err := (&net.Dialer{Timeout: sshCfg.Timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := gossh.NewClientConn(conn, addr, sshCfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &Comm{client: gossh.NewClient(c, chans, reqs)}, nil
}

// Run runs command through the remote user's shell. It returns *ExitError
// when the command exits with a non-zero status.
func (c *Comm) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	return c.RunInput(ctx, command, nil, stdout, stderr)
}

// RunInput is Run with stdin (nil for none): the provisioners of plugins
// stream data to their commands (Ansible's piped transfers).
func (c *Comm) RunInput(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	sess, err := c.client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdin, sess.Stdout, sess.Stderr = stdin, stdout, stderr

	done := make(chan error, 1)
	go func() { done <- sess.Run(command) }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(gossh.SIGTERM)
		sess.Close()
		return ctx.Err()
	case err := <-done:
		var exitErr *gossh.ExitError
		if errors.As(err, &exitErr) {
			return &ExitError{Code: exitErr.ExitStatus()}
		}
		return err
	}
}

// Upload copies the local file or directory src to the remote path dst,
// creating its parent directories and keeping the permission bits.
func (c *Comm) Upload(ctx context.Context, src, dst string) error {
	client, err := sftp.NewClient(c.client)
	if err != nil {
		return fmt.Errorf("sftp: %w", err)
	}
	defer client.Close()
	if err := client.MkdirAll(path.Dir(dst)); err != nil {
		return fmt.Errorf("mkdir %s: %w", path.Dir(dst), err)
	}
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := path.Join(dst, filepath.ToSlash(rel))
		switch {
		case info.IsDir():
			if err := client.MkdirAll(target); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		case info.Mode().IsRegular():
			if err := uploadFile(client, p, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: only regular files and directories can be copied", p)
		}
		return client.Chmod(target, info.Mode().Perm())
	})
}

func uploadFile(client *sftp.Client, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := client.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return out.Close()
}

// Close closes the connection.
func (c *Comm) Close() error { return c.client.Close() }
