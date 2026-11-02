package ssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

// startServer starts an in-process SSH server that accepts clientKey,
// runs "exec" requests with /bin/sh and serves the sftp subsystem.
func startServer(t *testing.T, clientKey gossh.PublicKey) (string, int) {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, err := gossh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &gossh.ServerConfig{
		PublicKeyCallback: func(_ gossh.ConnMetadata, key gossh.PublicKey) (*gossh.Permissions, error) {
			if bytes.Equal(key.Marshal(), clientKey.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveConn(conn, cfg)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

func serveConn(conn net.Conn, cfg *gossh.ServerConfig) {
	_, chans, reqs, err := gossh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go gossh.DiscardRequests(reqs)
	for nc := range chans {
		ch, requests, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for req := range requests {
				switch req.Type {
				case "exec":
					req.Reply(true, nil)
					cmd := exec.Command("/bin/sh", "-c", string(req.Payload[4:]))
					cmd.Stdout, cmd.Stderr = ch, ch.Stderr()
					code := uint32(0)
					var exitErr *exec.ExitError
					if err := cmd.Run(); errors.As(err, &exitErr) {
						code = uint32(exitErr.ExitCode())
					}
					status := make([]byte, 4)
					binary.BigEndian.PutUint32(status, code)
					ch.SendRequest("exit-status", false, status)
					return
				case "subsystem":
					req.Reply(true, nil)
					server, err := sftp.NewServer(ch)
					if err == nil {
						server.Serve()
					}
					return
				default:
					req.Reply(false, nil)
				}
			}
		}()
	}
}

func dial(t *testing.T) *Comm {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	host, port := startServer(t, signer.PublicKey())
	c, err := Dial(context.Background(), Config{Host: host, Port: port, User: "tester", Signer: signer})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestRun(t *testing.T) {
	c := dial(t)
	var stdout, stderr bytes.Buffer
	if err := c.Run(context.Background(), "echo out; echo err >&2", &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "out\n" || stderr.String() != "err\n" {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}

	err := c.Run(context.Background(), "exit 7", &stdout, &stderr)
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Errorf("got %v, want exit code 7", err)
	}
}

func TestDialRejectsUnknownKey(t *testing.T) {
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	otherSigner, _ := gossh.NewSignerFromKey(other)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := gossh.NewSignerFromKey(priv)
	host, port := startServer(t, otherSigner.PublicKey())
	if _, err := Dial(context.Background(), Config{Host: host, Port: port, User: "u", Signer: signer}); err == nil {
		t.Fatal("expected an authentication error")
	}
}

func TestUpload(t *testing.T) {
	c := dial(t)
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "dir", "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "dir", "a.sh"), []byte("#!/bin/sh\n"), 0o750)
	os.WriteFile(filepath.Join(src, "dir", "sub", "b"), []byte("b"), 0o600)
	os.WriteFile(filepath.Join(src, "file"), []byte("f"), 0o644)
	dst := t.TempDir()

	// A directory, and a single file into a directory that does not exist.
	if err := c.Upload(context.Background(), filepath.Join(src, "dir"), filepath.Join(dst, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := c.Upload(context.Background(), filepath.Join(src, "file"), filepath.Join(dst, "new", "file")); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		path    string
		content string
		mode    os.FileMode
	}{
		{"dir/a.sh", "#!/bin/sh\n", 0o750},
		{"dir/sub/b", "b", 0o600},
		{"new/file", "f", 0o644},
	}
	for _, ck := range checks {
		p := filepath.Join(dst, ck.path)
		data, err := os.ReadFile(p)
		if err != nil || string(data) != ck.content {
			t.Errorf("%s: %q, %v", ck.path, data, err)
			continue
		}
		if info, _ := os.Stat(p); info.Mode().Perm() != ck.mode {
			t.Errorf("%s: mode %v, want %v", ck.path, info.Mode().Perm(), ck.mode)
		}
	}
}
