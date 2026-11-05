package packer

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// defaultBuilder is the name of a builder registered as the plugin's only
// one: its source type is the plugin name alone.
const defaultBuilder = "-packer-default-plugin-name-"

// Plugin is a packer-plugin-<name> binary.
type Plugin struct {
	Name string
	// Version is empty for a binary found in PATH.
	Version string
	Path    string
}

// SearchDirs returns where plugins are looked up, in order: the
// directories of PACKER_PLUGIN_PATH, Packer's plugin directory (where
// `packer plugins install` puts them) and PATH.
func SearchDirs() []string {
	var dirs []string
	dirs = append(dirs, filepath.SplitList(os.Getenv("PACKER_PLUGIN_PATH"))...)
	if dir := PluginDir(); dir != "" {
		dirs = append(dirs, dir)
	}
	return dirs
}

// PluginDir returns Packer's plugin directory, where `packer plugins
// install` and `opendango plugin pull` put the plugins:
// $PACKER_CONFIG_DIR/plugins, or ~/.config/packer/plugins. It is empty when
// neither is known.
func PluginDir() string {
	configDir := os.Getenv("PACKER_CONFIG_DIR")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configDir = filepath.Join(home, ".config", "packer")
	}
	return filepath.Join(configDir, "plugins")
}

// Discover lists the plugin binaries of dirs (searched recursively, for the
// layout of `packer plugins install`) and of path (the PATH directories,
// not recursive), in that order.
func Discover(dirs, path []string) []Plugin {
	var plugins []Plugin
	for _, dir := range dirs {
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				// A missing or unreadable directory: skip it.
				return filepath.SkipDir
			}
			if !d.IsDir() {
				if pl, ok := parseName(p); ok {
					plugins = append(plugins, pl)
				}
			}
			return nil
		})
	}
	for _, dir := range path {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if pl, ok := parseName(filepath.Join(dir, e.Name())); ok && pl.Version == "" {
				plugins = append(plugins, pl)
			}
		}
	}
	return plugins
}

// parseName recognizes packer-plugin-<name> and the installed form
// packer-plugin-<name>_v<version>_x<api>_<os>_<arch>, for this OS and
// architecture only.
func parseName(p string) (Plugin, bool) {
	base := strings.TrimSuffix(filepath.Base(p), ".exe")
	name, ok := strings.CutPrefix(base, "packer-plugin-")
	if !ok || name == "" || strings.HasSuffix(name, "SHA256SUM") {
		return Plugin{}, false
	}
	pl := Plugin{Name: name, Path: p}
	if i := strings.Index(name, "_v"); i >= 0 {
		parts := strings.Split(name[i+1:], "_")
		if len(parts) != 4 || parts[2] != runtime.GOOS || parts[3] != runtime.GOARCH {
			return Plugin{}, false
		}
		pl.Name, pl.Version = name[:i], strings.TrimPrefix(parts[0], "v")
	}
	if info, err := os.Stat(p); err != nil || info.IsDir() || (runtime.GOOS != "windows" && info.Mode()&0o111 == 0) {
		return Plugin{}, false
	}
	return pl, true
}

// Find returns the plugin and builder of a source type such as
// xenserver-iso: the plugin with the longest matching name, then the first
// found, then the highest version.
func Find(plugins []Plugin, source string) (*Plugin, string, error) {
	var best *Plugin
	for i := range plugins {
		pl := &plugins[i]
		if source != pl.Name && !strings.HasPrefix(source, pl.Name+"-") {
			continue
		}
		switch {
		case best == nil, len(pl.Name) > len(best.Name):
			best = pl
		case len(pl.Name) == len(best.Name) && compareVersions(pl.Version, best.Version) > 0:
			best = pl
		}
	}
	if best == nil {
		return nil, "", fmt.Errorf("plugin: no plugin for %q: install it with `opendango plugin pull` (or `packer plugins install`), or put packer-plugin-<name> in PATH or PACKER_PLUGIN_PATH", source)
	}
	builder := strings.TrimPrefix(strings.TrimPrefix(source, best.Name), "-")
	if builder == "" {
		builder = defaultBuilder
	}
	return best, builder, nil
}

// compareVersions compares dotted numeric versions; an empty or invalid
// one is the lowest.
func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(strings.SplitN(pa[i], "-", 2)[0])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(strings.SplitN(pb[i], "-", 2)[0])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Description is the output of `packer-plugin-<name> describe`.
type Description struct {
	Version    string   `json:"version"`
	APIVersion string   `json:"api_version"`
	Builders   []string `json:"builders"`
}

// Describe runs the plugin's describe command.
func (p *Plugin) Describe(ctx context.Context) (*Description, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, p.Path, "describe").Output()
	if err != nil {
		return nil, fmt.Errorf("plugin: %s describe: %w", p.Path, err)
	}
	var d Description
	if err := json.Unmarshal(out, &d); err != nil {
		return nil, fmt.Errorf("plugin: %s describe: %w", p.Path, err)
	}
	if !strings.HasPrefix(d.APIVersion, "x"+apiMajor+".") {
		return nil, fmt.Errorf("plugin: %s speaks plugin API %s, opendango speaks x%s.x", p.Path, d.APIVersion, apiMajor)
	}
	return &d, nil
}
