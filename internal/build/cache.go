package build

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/littlejo/opendango/internal/instructions"
	"github.com/littlejo/opendango/internal/plan"
)

// cacheVersion changes when the meaning of the key changes, so that old
// cache entries stop matching.
const cacheVersion = "opendango-cache-v1"

// cacheKey returns the cache key of a stage: a hash of the parent's key and
// of everything that makes the stage's image, with the variables expanded
// and the content of the COPY sources and of the SERVE directory. Line
// numbers are left out. It returns "" when the stage cannot be cached: it,
// or an ancestor, has a BREAKPOINT (whatever is done by hand during the
// pause is not reproducible) or an ADD of a URL without --checksum (the
// content is unknown until downloaded).
func cacheKey(file string, s *plan.Stage, parentKey, contextDir string) (string, error) {
	return stageKey(file, s, map[*plan.Stage]string{s.Parent: parentKey}, contextDir)
}

// stageKey is cacheKey with the keys of the stages built before (the
// parent, and the stages of COPY --from): a COPY --from is keyed by its
// source stage's key and its paths, the files being what that stage
// builds. A stage that copies from a stage without a key has none.
func stageKey(file string, s *plan.Stage, keys map[*plan.Stage]string, contextDir string) (string, error) {
	parentKey := keys[s.Parent]
	if s.Parent != nil && parentKey == "" {
		return "", nil
	}
	for _, src := range s.CopyFrom {
		if keys[src] == "" {
			return "", nil
		}
	}
	byName := map[string]string{}
	for _, src := range s.CopyFrom {
		byName[src.Name()] = keys[src]
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\nparent %q\n", cacheVersion, parentKey)
	fmt.Fprintf(h, "FROM %q %q %q\n", s.From.Builder, s.From.Source, s.From.Stage)
	// A plugin:// stage: with its dango.yaml settings and inherited flags.
	values := s.From.Flags
	if s.IsPacker() {
		var err error
		if values, err = s.PackerFlags(); err != nil {
			return "", err
		}
	}
	flags := make([]string, 0, len(values))
	for k := range values {
		flags = append(flags, k)
	}
	sort.Strings(flags)
	for _, k := range flags {
		fmt.Fprintf(h, "  --%s=%q\n", k, values[k])
	}
	if c := s.Connect; c != nil {
		fmt.Fprintf(h, "CONNECT %q %q %d\n", c.Protocol, c.User, c.Port)
	}
	// The served files may end up in the image (kickstart, packages); the
	// port and address do not.
	if sv := s.Serve; sv != nil {
		fmt.Fprintf(h, "SERVE %q\n", sv.Dir)
		_, err := serveDir(contextDir, sv)
		if err == nil {
			err = hashTree(h, contextDir, filepath.Join(contextDir, filepath.FromSlash(sv.Dir)))
		}
		if err != nil {
			return "", &plan.Error{File: file, Line: sv.Line(), Err: err}
		}
	}
	// With the SERVE address as a marker: the port does not matter.
	for _, boot := range s.Boot {
		fmt.Fprintf(h, "BOOT %q\n", boot.Keys)
	}
	for _, step := range s.Steps {
		switch st := step.(type) {
		case *instructions.Arg:
			fmt.Fprintf(h, "ARG %q %v %q\n", st.Name, st.Set, st.Value)
		case *instructions.Env:
			fmt.Fprintf(h, "ENV %v", st.Persist)
			for _, kv := range st.Vars {
				fmt.Fprintf(h, " %q=%q", kv.Key, kv.Value)
			}
			fmt.Fprintln(h)
		case *instructions.User:
			fmt.Fprintf(h, "USER %q\n", st.Name)
		case *instructions.Shell:
			fmt.Fprintf(h, "SHELL %q\n", st.Args)
		case *instructions.Workdir:
			fmt.Fprintf(h, "WORKDIR %q\n", st.Path)
		case *instructions.Run:
			fmt.Fprintf(h, "RUN %q\n", st.Script)
		case *instructions.Copy:
			if st.From != "" {
				fmt.Fprintf(h, "COPY --from=%q %q %q %q %q\n", byName[st.From], st.Sources, st.Dest, st.Chmod, st.Chown)
				continue
			}
			fmt.Fprintf(h, "COPY %q %q %q %q\n", st.Sources, st.Dest, st.Chmod, st.Chown)
			if err := hashSources(h, contextDir, st.Sources); err != nil {
				return "", &plan.Error{File: file, Line: st.Line(), Err: fmt.Errorf("COPY: %w", err)}
			}
		case *instructions.Add:
			fmt.Fprintf(h, "ADD %q %q %q %q %q\n", st.Sources, st.Dest, st.Chmod, st.Chown, st.Checksum)
			var local []string
			for _, src := range st.Sources {
				switch {
				case !instructions.IsURL(src):
					local = append(local, src)
				case st.Checksum == "":
					// The content is unknown until downloaded: not cached.
					return "", nil
				}
			}
			if err := hashSources(h, contextDir, local); err != nil {
				return "", &plan.Error{File: file, Line: st.Line(), Err: fmt.Errorf("ADD: %w", err)}
			}
		case *instructions.Reboot:
			fmt.Fprintln(h, "REBOOT")
		case *instructions.Breakpoint:
			return "", nil
		default:
			return "", fmt.Errorf("cache key: unexpected instruction %T", step)
		}
	}
	// The cached template carries the OUTPUT name and the labels (tags): a
	// new name or label is a new template.
	if out := s.TemplateOutput(); out != nil {
		fmt.Fprintf(h, "OUTPUT template %q\n", out.Name)
		for _, l := range s.Labels {
			fmt.Fprintf(h, "LABEL %q=%q\n", l.Key, l.Value)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashSources hashes the files of COPY sources (glob patterns).
func hashSources(h hash.Hash, contextDir string, patterns []string) error {
	for _, pattern := range patterns {
		matches, err := resolveSource(contextDir, pattern)
		if err != nil {
			return err
		}
		for _, m := range matches {
			if err := hashTree(h, contextDir, m); err != nil {
				return err
			}
		}
	}
	return nil
}

// hashTree writes the path (relative to contextDir), mode and content of
// root and of everything under it, in lexical order.
func hashTree(h hash.Hash, contextDir, root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(contextDir, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "  %q %v", filepath.ToSlash(rel), info.Mode())
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			content := sha256.New()
			if _, err := io.Copy(content, f); err != nil {
				return err
			}
			fmt.Fprintf(h, " %x", content.Sum(nil))
		}
		fmt.Fprintln(h)
		return nil
	})
}
