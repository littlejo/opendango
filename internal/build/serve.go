package build

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/littlejo/opendango/internal/instructions"
)

// serveDir returns the absolute path of the SERVE directory, which must
// exist.
func serveDir(contextDir string, sv *instructions.Serve) (string, error) {
	// Absolute: a plugin runs in contextDir, not in the current directory.
	dir, err := filepath.Abs(filepath.Join(contextDir, filepath.FromSlash(sv.Dir)))
	if err != nil {
		return "", fmt.Errorf("SERVE: %w", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("SERVE: %s is not a directory", sv.Dir)
	}
	return dir, nil
}

// serve starts the HTTP server of SERVE and returns the address the VM
// reaches it at (ip:port) and the function that stops it. It listens on
// every interface: --addr may be a NAT address. The files are read through
// os.Root, so symbolic links cannot escape the directory.
func (e *executor) serve(ctx context.Context, sv *instructions.Serve) (addr string, stop func(), err error) {
	dir, err := serveDir(e.contextDir, sv)
	if err != nil {
		return "", nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", nil, fmt.Errorf("SERVE: %w", err)
	}
	ip := sv.Addr
	if ip == "" {
		if ip, err = e.b.LocalAddr(ctx); err != nil {
			root.Close()
			return "", nil, fmt.Errorf("SERVE: %w (set --addr)", err)
		}
	}
	l, err := net.Listen("tcp", ":"+strconv.Itoa(sv.Port))
	if err != nil {
		root.Close()
		return "", nil, fmt.Errorf("SERVE: %w", err)
	}
	files := http.FileServerFS(root.FS())
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			e.say("SERVE: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
			files.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 30 * time.Second,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			e.say("SERVE: %v", err)
		}
	}()
	addr = net.JoinHostPort(ip, strconv.Itoa(l.Addr().(*net.TCPAddr).Port))
	e.say("Serving %s at http://%s/", sv.Dir, addr)
	return addr, func() {
		server.Close()
		<-done
		root.Close()
	}, nil
}
