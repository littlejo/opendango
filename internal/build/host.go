package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/littlejo/opendango/internal/instructions"
)

// retry runs attempt, again up to r.Retries times after a failure, each
// attempt stopped after r.Timeout. The error is the last attempt's.
func (e *executor) retry(ctx context.Context, kw string, r instructions.Retry, attempt func(context.Context) error) error {
	for i := 0; ; i++ {
		actx, cancel := ctx, context.CancelFunc(func() {})
		if r.Timeout > 0 {
			actx, cancel = context.WithTimeout(ctx, r.Timeout)
		}
		err := attempt(actx)
		if err != nil && actx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			err = fmt.Errorf("timeout after %s: %w", r.Timeout, err)
		}
		cancel()
		if err == nil || i >= r.Retries || ctx.Err() != nil {
			return err
		}
		e.say("%s failed (%v), retrying (%d/%d)", kw, err, i+1, r.Retries)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(e.retryDelay):
		}
	}
}

// runLocal runs a RUN --local script with /bin/sh on the machine that runs
// opendango, in the Dangofile directory, with the ARG and ENV values (and
// DANGO_HTTP_ADDR) in its environment. USER, WORKDIR and SHELL are the
// VM's: they do not apply.
func (e *executor) runLocal(ctx context.Context, script string) error {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Dir = e.contextDir
	cmd.Env = os.Environ()
	if e.serveAddr != "" {
		cmd.Env = append(cmd.Env, instructions.ServeAddrVar+"="+e.serveAddr)
	}
	// ENV after ARG: on a name clash, ENV wins.
	for _, kv := range append(append([]instructions.KeyValue{}, e.args...), e.env...) {
		cmd.Env = append(cmd.Env, kv.Key+"="+kv.Value)
	}
	w := newPrefixWriter(e.out, "    "+e.name+": ")
	defer w.Flush()
	cmd.Stdout, cmd.Stderr = w, w
	// Do not wait for the children that keep the output open once the
	// shell is killed.
	cmd.WaitDelay = time.Second
	return cmd.Run()
}

// fetchHost runs FETCH: the sources are read as root in the VM (tar, like
// COPY --from), extracted to a temporary directory, then placed under the
// destination like COPY places its sources: a directory source gives its
// contents, the destination is a directory when it ends with / or when
// there are several sources.
func (e *executor) fetchHost(ctx context.Context, f *instructions.Fetch) error {
	tmp, err := os.MkdirTemp("", "opendango-fetch-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := e.tarFrom(ctx, f.Sources, tmp); err != nil {
		return err
	}
	dest := filepath.FromSlash(f.Dest)
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(e.contextDir, dest)
	}
	toDir := strings.HasSuffix(f.Dest, "/") || len(f.Sources) > 1
	for _, src := range f.Sources {
		local := filepath.Join(tmp, filepath.FromSlash(src))
		info, err := os.Lstat(local)
		if err != nil {
			return fmt.Errorf("%s: not found in the VM", src)
		}
		target := dest
		if d, err := os.Stat(dest); !info.IsDir() && (toDir || err == nil && d.IsDir()) {
			target = filepath.Join(dest, filepath.Base(local))
		}
		if err := copyLocal(local, target); err != nil {
			return err
		}
	}
	return nil
}

// copyLocal copies src to dst: a directory's contents into dst (created),
// a file or a symbolic link to dst itself, replacing what is there.
func copyLocal(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Remove(target)
			return os.Symlink(link, target)
		default:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			r, err := os.Open(p)
			if err != nil {
				return err
			}
			defer r.Close()
			w, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm()|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(w, r)
			return errors.Join(err, w.Close())
		}
	})
}
