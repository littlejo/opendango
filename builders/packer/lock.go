package packer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"
)

// LockFile is the name of the lock file read next to the Dangofile.
const LockFile = "dango.lock"

// Lock is a dango.lock file: the plugins of a Dangofile, each at one
// version, with the sha256 of its binary for each platform it was pulled
// on. `opendango plugin pull` writes it; when it exists, the builds only
// use the plugins it lists, at their version, and check their hash.
type Lock struct {
	// File is the path of the file, for the messages.
	File    string                   `yaml:"-"`
	Plugins map[string]*LockedPlugin `yaml:"plugins"`

	mu     sync.Mutex
	hashed map[string]string
}

// LockedPlugin is a plugin of the lock file.
type LockedPlugin struct {
	// Source is github.com/<owner>/<name>.
	Source  string `yaml:"source"`
	Version string `yaml:"version"`
	// Hashes are the sha256 of the binary by platform (linux_amd64).
	Hashes map[string]string `yaml:"hashes"`
}

// Platform is the key of the hashes of this machine: <os>_<arch>.
func Platform() string { return runtime.GOOS + "_" + runtime.GOARCH }

// ReadLock reads a lock file; a missing file is a nil lock and no error.
func ReadLock(path string) (*Lock, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	l := &Lock{File: path}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(l); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for name, p := range l.Plugins {
		if p == nil || p.Version == "" {
			return nil, fmt.Errorf("%s: plugin %s has no version", path, name)
		}
		if _, src, _, err := ParseRef(p.Source); err != nil || src == nil {
			return nil, fmt.Errorf("%s: plugin %s: invalid plugin source %q: expected github.com/<owner>/<name>", path, name, p.Source)
		}
	}
	return l, nil
}

// Write writes the lock file, the plugins sorted by name.
func (l *Lock) Write() error {
	var b strings.Builder
	b.WriteString("# Written by `opendango plugin pull`: the plugins of the Dangofile, at the\n# version the builds use. Commit it; `opendango plugin pull --upgrade` updates it.\n")
	if len(l.Plugins) == 0 {
		b.WriteString("plugins: {}\n")
		return os.WriteFile(l.File, []byte(b.String()), 0o644)
	}
	names := make([]string, 0, len(l.Plugins))
	for name := range l.Plugins {
		names = append(names, name)
	}
	sort.Strings(names)
	b.WriteString("plugins:\n")
	for _, name := range names {
		p := l.Plugins[name]
		fmt.Fprintf(&b, "  %s:\n    source: %s\n    version: %s\n    hashes:\n", name, p.Source, yamlString(p.Version))
		platforms := make([]string, 0, len(p.Hashes))
		for platform := range p.Hashes {
			platforms = append(platforms, platform)
		}
		sort.Strings(platforms)
		for _, platform := range platforms {
			fmt.Fprintf(&b, "      %s: %s\n", platform, p.Hashes[platform])
		}
	}
	return os.WriteFile(l.File, []byte(b.String()), 0o644)
}

// yamlString quotes a version that YAML would read as a number (1.2).
func yamlString(s string) string {
	var v any
	if err := yaml.Unmarshal([]byte(s), &v); err == nil {
		if _, ok := v.(string); ok {
			return s
		}
	}
	return `"` + s + `"`
}

// Set records a plugin at a version, with the hash of its binary for this
// platform. The hashes of the other platforms stay when the version and
// the source do not change.
func (l *Lock) Set(pl *Plugin, source string) error {
	sum, err := fileSHA256(pl.Path)
	if err != nil {
		return err
	}
	if l.Plugins == nil {
		l.Plugins = map[string]*LockedPlugin{}
	}
	p := l.Plugins[pl.Name]
	if p == nil || p.Version != pl.Version || p.Source != source {
		p = &LockedPlugin{Source: source, Version: pl.Version, Hashes: map[string]string{}}
		l.Plugins[pl.Name] = p
	}
	p.Hashes[Platform()] = sum
	return nil
}

// Find is the Find of the plugins the lock allows: the plugin of the
// source type must be in the lock, installed at its version, with the hash
// of this platform.
func (l *Lock) Find(plugins []Plugin, source string) (*Plugin, string, error) {
	best, _, err := Find(plugins, source)
	if err != nil {
		return nil, "", err
	}
	locked, ok := l.Plugins[best.Name]
	if !ok {
		return nil, "", fmt.Errorf("plugin: %s (for %s) is not in %s: run `opendango plugin pull`", best.Name, source, l.File)
	}
	var allowed []Plugin
	for _, pl := range plugins {
		if pl.Name == best.Name && pl.Version == locked.Version {
			allowed = append(allowed, pl)
		}
	}
	if len(allowed) == 0 {
		return nil, "", fmt.Errorf("plugin: %s %s (%s) is not installed: run `opendango plugin pull`", best.Name, locked.Version, l.File)
	}
	pl, name, err := Find(allowed, source)
	if err != nil {
		return nil, "", err
	}
	want, ok := locked.Hashes[Platform()]
	if !ok {
		return nil, "", fmt.Errorf("plugin: %s has no hash of %s for %s: run `opendango plugin pull` on this platform", l.File, best.Name, Platform())
	}
	sum, err := l.hash(pl.Path)
	if err != nil {
		return nil, "", err
	}
	if sum != want {
		return nil, "", fmt.Errorf("plugin: %s: sha256 %s, %s has %s", pl.Path, sum, l.File, want)
	}
	return pl, name, nil
}

// hash returns the sha256 of a binary, once per path.
func (l *Lock) hash(path string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if sum, ok := l.hashed[path]; ok {
		return sum, nil
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return "", err
	}
	if l.hashed == nil {
		l.hashed = map[string]string{}
	}
	l.hashed[path] = sum
	return sum, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Pull installs the plugins of the source types (xenserver-iso, ansible) of
// a Dangofile and records them in the lock: at their locked version, or the
// latest one when they are not locked yet or with upgrade. The plugins
// that no source type uses anymore leave the lock. installed are the
// plugins found on this machine: they tell which plugin a source type
// belongs to before the registry does.
func (r *Registry) Pull(ctx context.Context, l *Lock, sources []string, upgrade bool, installed []Plugin, out io.Writer) error {
	used := map[string]bool{}
	for _, source := range sources {
		src, err := r.sourceOf(ctx, l, source, installed)
		if err != nil {
			return fmt.Errorf("%s: %w", source, err)
		}
		if used[src.Name] {
			continue
		}
		used[src.Name] = true
		version := ""
		if p, ok := l.Plugins[src.Name]; ok && !upgrade && p.Source == src.String() {
			version = p.Version
		}
		pl, isNew, err := r.Install(ctx, src, version)
		if err != nil {
			return fmt.Errorf("%s: %w", src, err)
		}
		if isNew {
			fmt.Fprintf(out, "Installed %s %s: %s\n", src, pl.Version, pl.Path)
		} else {
			fmt.Fprintf(out, "%s %s is installed: %s\n", src, pl.Version, pl.Path)
		}
		if err := l.Set(pl, src.String()); err != nil {
			return err
		}
	}
	for name := range l.Plugins {
		if !used[name] {
			fmt.Fprintf(out, "Removing %s from %s: the Dangofile does not use it\n", name, l.File)
			delete(l.Plugins, name)
		}
	}
	return nil
}

// sourceOf returns the plugin of a source type: the locked plugin whose
// name it starts with (the longest), else the installed one, else the
// registry's plugin of the longest name it starts with (amazon-ebs:
// amazon-ebs, then amazon).
func (r *Registry) sourceOf(ctx context.Context, l *Lock, source string, installed []Plugin) (Source, error) {
	best := ""
	for name := range l.Plugins {
		if (source == name || strings.HasPrefix(source, name+"-")) && len(name) > len(best) {
			best = name
		}
	}
	if best != "" {
		_, src, _, err := ParseRef(l.Plugins[best].Source)
		return *src, err
	}
	if pl, _, err := Find(installed, source); err == nil {
		if src, ok := SourceOf(pl); ok {
			return src, nil
		}
	}
	parts := strings.Split(source, "-")
	for i := len(parts); i > 0; i-- {
		src, err := r.Resolve(ctx, strings.Join(parts[:i], "-"))
		if err == nil {
			return src, nil
		}
		if !errors.Is(err, ErrNoPlugin) {
			return Source{}, err
		}
	}
	return Source{}, fmt.Errorf("%w %q or a prefix of it: install it with `opendango plugin pull github.com/<owner>/<name>`", ErrNoPlugin, source)
}

// SourceOf returns the source of a plugin installed in Packer's layout
// (github.com/<owner>/<name>/), false otherwise (a binary in PATH).
func SourceOf(pl *Plugin) (Source, bool) {
	parts := strings.Split(filepath.ToSlash(pl.Path), "/")
	for i := 0; i+3 < len(parts); i++ {
		if parts[i] == "github.com" && parts[i+2] == pl.Name {
			return Source{Owner: parts[i+1], Name: pl.Name}, true
		}
	}
	return Source{}, false
}
