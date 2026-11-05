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
	"slices"
	"strings"
	"time"

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

// filesCache keeps the files fetched for COPY --from by cache key, so that
// a later build need not start the source stage's machine:
// <dir>/<key>/paths lists the fetched paths, <dir>/<key>/root holds them.
// The zero value keeps nothing.
type filesCache string

func (c filesCache) root(key string) string { return filepath.Join(string(c), key, "root") }

// has tells whether every path was fetched for key.
func (c filesCache) has(key string, paths []string) bool {
	if c == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(string(c), key, "paths"))
	if err != nil {
		return false
	}
	have := strings.Split(strings.TrimSpace(string(b)), "\n")
	for _, p := range paths {
		if !slices.Contains(have, p) {
			return false
		}
	}
	return true
}

// tmp returns a new directory to fetch into, next to the entries (commit
// renames it).
func (c filesCache) tmp(key string) (string, error) {
	if err := os.MkdirAll(string(c), 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(string(c), key+".tmp-")
}

// commit makes the fetched directory tmp the entry of key, replacing the
// previous one, and returns its root.
func (c filesCache) commit(key, tmp string, paths []string) (string, error) {
	if err := os.WriteFile(filepath.Join(tmp, "paths"), []byte(strings.Join(paths, "\n")+"\n"), 0o600); err != nil {
		return "", err
	}
	entry := filepath.Join(string(c), key)
	if err := os.RemoveAll(entry); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, entry); err != nil {
		return "", err
	}
	return c.root(key), nil
}

// fetchFiles fetches the files later stages COPY --from this one: into the
// files cache when the stage has a key (and no instruction was retried by
// hand), otherwise into the build's directory.
func (e *executor) fetchFiles(ctx context.Context, stage *plan.Stage) error {
	if len(stage.Fetch) == 0 {
		return nil
	}
	if e.filesKey == "" || e.retried || e.files == "" {
		return e.fetch(ctx, stage, e.fetchRoot)
	}
	tmp, err := e.files.tmp(e.filesKey)
	if err != nil {
		return err
	}
	if err := e.fetch(ctx, stage, filepath.Join(tmp, "root")); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	root, err := e.files.commit(e.filesKey, tmp, stage.Fetch)
	if err != nil {
		os.RemoveAll(tmp)
		return err
	}
	e.fetchRoot = root
	return nil
}

// fetchStage is a stage that starts from stage, without instructions: the
// machine that fetches the files of a stage found in the cache.
func fetchStage(stage *plan.Stage) *plan.Stage {
	return &plan.Stage{From: &instructions.From{Stage: stage.Name()}, Parent: stage, Connect: stage.Connect, Index: stage.Index}
}

// fetchFromTemplate fetches the files of a stage found in the cache as a
// template, without its files: a temporary VM starts from the template, the
// files are fetched, the VM is deleted.
func fetchFromTemplate(ctx context.Context, stage *plan.Stage, templateID string, e *executor, opts Options) (err error) {
	e.say("The files of COPY --from are not in the cache: fetching them from the cached template %s", templateID)
	b, err := opts.NewBuilder(fetchStage(stage), templateID)
	if err != nil {
		return err
	}
	fe := &executor{file: e.file, name: e.name, out: e.out, b: b, connect: stage.Connect, dial: e.dial, retryDelay: e.retryDelay,
		contextDir: e.contextDir, console: e.console, filesKey: e.filesKey, files: e.files, fetchRoot: e.fetchRoot}
	authorizedKey, err := fe.generateKey()
	if err != nil {
		return err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
		defer cancel()
		fe.say("Destroying VM %s", b.ID())
		if derr := b.Destroy(cctx); derr != nil {
			err = errors.Join(err, fmt.Errorf("destroying VM %s: %w", b.ID(), derr))
		}
	}()
	fe.say("Starting VM")
	if err := b.Start(ctx, CloudConfig(stage.Connect.User, authorizedKey)); err != nil {
		return fmt.Errorf("starting VM: %w", err)
	}
	if err := fe.open(ctx); err != nil {
		return err
	}
	defer fe.comm.Close()
	if err := fe.fetchFiles(ctx, stage); err != nil {
		return err
	}
	e.fetchRoot = fe.fetchRoot
	return nil
}
