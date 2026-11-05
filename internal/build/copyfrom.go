package build

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/littlejo/opendango/internal/instructions"
	"github.com/littlejo/opendango/internal/plan"
)

// fetch copies the paths that later stages COPY --from this one out of the
// machine, before it stops: tar runs as root in the machine and its stream
// is extracted under root (the machine's / is root). The same code works
// through SSH and through a plugin's communicator.
func (e *executor) fetch(ctx context.Context, stage *plan.Stage, root string) error {
	if len(stage.Fetch) == 0 {
		return nil
	}
	e.say("Fetching %s for COPY --from=%s", strings.Join(stage.Fetch, " "), stage.Name())
	rel := make([]string, len(stage.Fetch))
	for i, p := range stage.Fetch {
		rel[i] = instructions.ShellQuote(strings.TrimPrefix(p, "/"))
	}
	cmd := e.asRoot("tar -C / -cf - -- " + strings.Join(rel, " "))

	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	pr, pw := io.Pipe()
	extracted := make(chan error, 1)
	go func() {
		err := untar(pr, root)
		// Drain the rest so that the command does not block.
		io.Copy(io.Discard, pr)
		extracted <- err
	}()
	stderr := newPrefixWriter(e.out, "    "+e.name+": ")
	err := e.comm.Run(ctx, cmd, pw, stderr)
	stderr.Flush()
	pw.CloseWithError(err)
	if xerr := <-extracted; err == nil {
		err = xerr
	}
	if err != nil {
		return fmt.Errorf("fetching %s for COPY --from: %w", strings.Join(stage.Fetch, " "), err)
	}
	return nil
}

// untar extracts a tar stream under root: directories, regular files and
// symbolic links, with their modes. An entry that would land outside root
// is an error.
func untar(r io.Reader, root string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean("/" + h.Name)
		if name == "/" {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(name))
		if !strings.HasPrefix(target, filepath.Clean(root)+string(filepath.Separator)) {
			return fmt.Errorf("tar entry %q is outside of the copy", h.Name)
		}
		mode := os.FileMode(h.Mode).Perm()
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			if err := os.Chmod(target, mode|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		default:
			// Devices, FIFOs, hard links: not copied.
		}
	}
}

// copyFrom runs COPY --from=<stage>: its sources were fetched at the end of
// that stage, under root; they are uploaded like the files of COPY.
func (e *executor) copyFrom(ctx context.Context, c *instructions.Copy) error {
	root, ok := e.fetched[c.From]
	if !ok {
		return fmt.Errorf("--from=%s: the files of stage %s were not fetched", c.From, c.From)
	}
	srcs := make([]string, len(c.Sources))
	for i, src := range c.Sources {
		srcs[i] = filepath.Join(root, filepath.FromSlash(src))
		if _, err := os.Lstat(srcs[i]); err != nil {
			return fmt.Errorf("--from=%s: %s: %w", c.From, src, err)
		}
	}
	return e.place(ctx, c, srcs, true)
}

// uploadTar sends srcs in one tar archive, extracted in dir of the
// machine: source i lands at dir/i/<its name>, like the uploads of COPY.
// Symbolic links are kept, not followed.
func (e *executor) uploadTar(ctx context.Context, srcs []string, dir string) error {
	f, err := os.CreateTemp("", "opendango-copy-*.tar")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	tw := tar.NewWriter(f)
	for i, src := range srcs {
		base := filepath.Dir(src)
		prefix := fmt.Sprint(i)
		err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			link := ""
			if info.Mode()&os.ModeSymlink != 0 {
				if link, err = os.Readlink(p); err != nil {
					return err
				}
			}
			h, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(base, p)
			if err != nil {
				return err
			}
			h.Name = path.Join(prefix, filepath.ToSlash(rel))
			h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			r, err := os.Open(p)
			if err != nil {
				return err
			}
			defer r.Close()
			_, err = io.Copy(tw, r)
			return err
		})
		if err != nil {
			f.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	remote := path.Join(dir, "copy.tar")
	if err := e.comm.Upload(ctx, f.Name(), remote); err != nil {
		return fmt.Errorf("uploading the files: %w", err)
	}
	q := instructions.ShellQuote
	return e.run(ctx, "tar -C "+q(dir)+" -xf "+q(remote)+" && rm -f "+q(remote))
}
