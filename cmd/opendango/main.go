// Command opendango builds VM images described by a Dangofile.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/littlejo/opendango/builders/packer"
	"github.com/littlejo/opendango/builders/xo"
	"github.com/littlejo/opendango/internal/build"
	"github.com/littlejo/opendango/internal/format"
	"github.com/littlejo/opendango/internal/parser"
	"github.com/littlejo/opendango/internal/plan"
)

var version = "dev"

const usage = `Usage: opendango <command> [flags]

Commands:
  build      Build the image described by a Dangofile
  validate   Check a Dangofile without building it
  fmt        Rewrite a Dangofile in its canonical layout
  cache      Manage the cache of stage templates (cache prune)
  plugin     Manage the Packer plugins (pull, search, list, describe, rm)
  init       Write a dango.yaml with the settings of the plugins
  version    Print the version

Run 'opendango <command> -h' for the flags of a command.
`

func main() {
	// packer-plugin-sdk logs its internals with the standard log package;
	// opendango does not use it.
	log.SetOutput(io.Discard)
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "build":
		return cmdBuild(args[1:], stdout, stderr)
	case "validate":
		return cmdValidate(args[1:], stdout, stderr)
	case "fmt":
		return cmdFmt(args[1:], stdout, stderr)
	case "cache":
		return cmdCache(args[1:], stdout, stderr)
	case "plugin":
		return cmdPlugin(args[1:], stdout, stderr)
	case "init":
		return cmdInit(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, "opendango", version)
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "opendango: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// buildArgs collects repeated --build-arg KEY=VALUE flags.
type buildArgs map[string]string

func (b buildArgs) String() string { return "" }

func (b buildArgs) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("expected KEY=VALUE, got %q", s)
	}
	b[k] = v
	return nil
}

type commonFlags struct {
	file      string
	config    string
	buildArgs buildArgs
}

func newFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *commonFlags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := &commonFlags{buildArgs: buildArgs{}}
	fs.StringVar(&c.file, "f", "Dangofile", "path to the Dangofile")
	fs.Var(c.buildArgs, "build-arg", "set a build variable, KEY=VALUE (repeatable)")
	fs.StringVar(&c.config, "config", "", "plugin settings file (default: "+plan.SettingsFile+" next to the Dangofile, when it exists)")
	return fs, c
}

// loadSettings reads --config, or dango.yaml next to the Dangofile when it
// exists (nil otherwise).
func loadSettings(c *commonFlags) (*plan.Settings, error) {
	path := c.config
	if path == "" {
		path = filepath.Join(filepath.Dir(c.file), plan.SettingsFile)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
	}
	return plan.LoadSettings(path)
}

func loadPlan(c *commonFlags) (*plan.Plan, error) {
	f, err := os.Open(c.file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	res, err := parser.Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.file, err)
	}
	settings, err := loadSettings(c)
	if err != nil {
		return nil, err
	}
	return plan.BuildSettings(c.file, res, c.buildArgs, settings)
}

func cmdValidate(args []string, stdout, stderr io.Writer) int {
	fs, c := newFlagSet("validate", stderr)
	offline := fs.Bool("offline", false, "do not check the FROM flags of plugin:// stages with their plugins")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "opendango validate: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	p, err := loadPlan(c)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	for _, w := range p.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}
	if !*offline {
		// The plugins check their settings (Prepare creates nothing).
		warnings, err := build.CheckPlugins(context.Background(), p, filepath.Dir(c.file), nil)
		for _, w := range warnings {
			fmt.Fprintf(stderr, "warning: %s\n", w)
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	fmt.Fprintf(stdout, "%s is valid\n", c.file)
	return 0
}

func cmdBuild(args []string, stdout, stderr io.Writer) int {
	fs, c := newFlagSet("build", stderr)
	onError := fs.String("on-error", "abort", "on failure: abort (destroy the VM and the intermediate templates), keep (leave them for debugging) or ask (retry, clean up or keep, from a terminal)")
	target := fs.String("target", "", "build only this stage and the stages it starts from")
	jobs := fs.Int("j", 1, "number of stages built at the same time")
	noCache := fs.Bool("no-cache", false, "build every stage, without using or filling the cache")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "opendango build: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *jobs < 1 {
		fmt.Fprintf(stderr, "opendango build: -j must be at least 1\n")
		return 2
	}
	if *onError != "abort" && *onError != "keep" && *onError != "ask" {
		fmt.Fprintf(stderr, "opendango build: --on-error must be abort, keep or ask\n")
		return 2
	}
	// Not a mere character device check: /dev/null is one too.
	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	if *onError == "ask" && !interactive {
		fmt.Fprintf(stderr, "opendango build: --on-error=ask needs a terminal\n")
		return 2
	}
	p, err := loadPlan(c)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	for _, w := range p.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}

	opts := build.Options{
		ContextDir:  filepath.Dir(c.file),
		Target:      *target,
		KeepOnError: *onError == "keep",
		Ask:         *onError == "ask",
		Jobs:        *jobs,
		NoCache:     *noCache,
		Out:         stdout,
	}
	// BREAKPOINT and --on-error=ask wait for input only when someone can
	// type it.
	if interactive {
		opts.Stdin = os.Stdin
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = build.Run(ctx, p, opts)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, "build interrupted")
		} else {
			fmt.Fprintf(stderr, "build failed: %v\n", err)
		}
		return 1
	}
	return 0
}

func cmdFmt(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fmt", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("f", "Dangofile", "path to the Dangofile")
	write := fs.Bool("w", false, "write the result to the file instead of stdout")
	check := fs.Bool("check", false, "print nothing, exit with code 1 if the file is not formatted")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "opendango fmt: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *write && *check {
		fmt.Fprintln(stderr, "opendango fmt: -w and --check cannot be used together")
		return 2
	}
	src, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	out, err := format.Format(src)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", *file, err)
		return 1
	}
	switch {
	case *check:
		if !bytes.Equal(src, out) {
			fmt.Fprintf(stderr, "%s is not formatted: run opendango fmt -w -f %s\n", *file, *file)
			return 1
		}
	case *write:
		if bytes.Equal(src, out) {
			return 0
		}
		info, err := os.Stat(*file)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := os.WriteFile(*file, out, info.Mode().Perm()); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	default:
		stdout.Write(out)
	}
	return 0
}

func cmdCache(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "prune" {
		fmt.Fprintln(stderr, "Usage: opendango cache prune [--older-than=DURATION] [--dry-run]")
		return 2
	}
	fs := flag.NewFlagSet("cache prune", flag.ContinueOnError)
	fs.SetOutput(stderr)
	olderThan := fs.Duration("older-than", 0, "only delete the templates older than this (e.g. 168h)")
	dryRun := fs.Bool("dry-run", false, "print what would be deleted, delete nothing")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "opendango cache prune: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *olderThan < 0 {
		fmt.Fprintln(stderr, "opendango cache prune: --older-than must be positive")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client, err := xo.NewClientFromEnv()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	entries, err := xo.ListCache(ctx, client)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// Only the intermediate templates: the OUTPUT templates are never
	// deleted.
	deleted := 0
	for _, e := range entries {
		if time.Since(e.Created) < *olderThan {
			continue
		}
		verb := "Deleting"
		if *dryRun {
			verb = "Would delete"
		}
		fmt.Fprintf(stdout, "%s %s (%s), created %s\n", verb, e.Name, e.ID, e.Created.Format(time.RFC3339))
		if *dryRun {
			continue
		}
		if err := xo.DeleteCache(ctx, client, e.ID); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		deleted++
	}
	if !*dryRun {
		fmt.Fprintf(stdout, "%d cache template(s) deleted\n", deleted)
	}
	return 0
}

const pluginUsage = `Usage:
  opendango plugin pull <name|github.com/owner/name>[@version]...
  opendango plugin search <term>
  opendango plugin list
  opendango plugin describe <source> [--search <text>]
  opendango plugin rm <name|github.com/owner/name>[@version]...

A name is the official HashiCorp plugin of that name, or else the only
GitHub repository named packer-plugin-<name>. Plugins are installed in
Packer's plugin directory (` + "`$PACKER_CONFIG_DIR/plugins` or `~/.config/packer/plugins`" + `).
GITHUB_TOKEN, when set, raises the rate limits of the GitHub API.
`

func cmdPlugin(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, pluginUsage)
		return 2
	}
	cmd, args := args[0], args[1:]
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(stdout, pluginUsage)
		return 0
	}
	switch {
	case cmd == "-h", cmd == "--help", cmd == "help":
		fmt.Fprint(stdout, pluginUsage)
		return 0
	case cmd == "list" && len(args) == 0:
		return pluginList(stdout)
	case cmd == "describe":
		return pluginDescribe(args, stdout, stderr)
	case cmd == "search" && len(args) == 1:
	case (cmd == "pull" || cmd == "rm") && len(args) > 0:
	default:
		fmt.Fprint(stderr, pluginUsage)
		return 2
	}
	reg, err := packer.NewRegistry()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch cmd {
	case "search":
		return pluginSearch(ctx, reg, args[0], stdout, stderr)
	case "rm":
		return pluginRemove(reg, args, stdout, stderr)
	}
	for _, ref := range args {
		if err := pluginPull(ctx, reg, ref, stdout); err != nil {
			fmt.Fprintf(stderr, "opendango plugin pull %s: %v\n", ref, err)
			return 1
		}
	}
	return 0
}

func pluginPull(ctx context.Context, reg *packer.Registry, ref string, stdout io.Writer) error {
	name, src, version, err := packer.ParseRef(ref)
	if err != nil {
		return err
	}
	if src == nil {
		s, err := reg.Resolve(ctx, name)
		if err != nil {
			return err
		}
		src = &s
	}
	pl, installed, err := reg.Install(ctx, *src, version)
	if err != nil {
		return err
	}
	if installed {
		fmt.Fprintf(stdout, "Installed %s %s: %s\n", src, pl.Version, pl.Path)
	} else {
		fmt.Fprintf(stdout, "%s %s is already installed: %s\n", src, pl.Version, pl.Path)
	}
	return nil
}

func pluginSearch(ctx context.Context, reg *packer.Registry, term string, stdout, stderr io.Writer) int {
	results, err := reg.Search(ctx, term)
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	if len(results) > 0 {
		fmt.Fprintln(w, "SOURCE\tSTARS\tDESCRIPTION")
	}
	for _, r := range results {
		desc := r.Description
		switch {
		case r.Official:
			desc = strings.TrimSpace("[official] " + desc)
		case r.Fork:
			desc = strings.TrimSpace("[fork] " + desc)
		}
		fmt.Fprintf(w, "%s\t%d\t%s\n", r.Source, r.Stars, desc)
	}
	w.Flush()
	if err != nil {
		fmt.Fprintf(stderr, "opendango plugin search: %v\n", err)
		return 1
	}
	if len(results) == 0 {
		fmt.Fprintf(stderr, "no plugin matches %q\n", term)
		return 1
	}
	return 0
}

// pluginList prints the plugins that a plugin:// stage can use, in the
// order they are looked up.
func pluginList(stdout io.Writer) int {
	plugins := packer.Discover(packer.SearchDirs(), filepath.SplitList(os.Getenv("PATH")))
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tVERSION\tPATH")
	for _, pl := range plugins {
		fmt.Fprintf(w, "%s\t%s\t%s\n", pl.Name, pl.Version, pl.Path)
	}
	w.Flush()
	return 0
}

func pluginRemove(reg *packer.Registry, refs []string, stdout, stderr io.Writer) int {
	for _, ref := range refs {
		name, src, version, err := packer.ParseRef(ref)
		if err == nil {
			var removed []string
			removed, err = reg.Remove(name, src, version)
			for _, p := range removed {
				fmt.Fprintf(stdout, "Removed %s\n", p)
			}
		}
		if err != nil {
			fmt.Fprintf(stderr, "opendango plugin rm %s: %v\n", ref, err)
			return 1
		}
	}
	return 0
}

// pluginDescribe prints the settings of a builder: the FROM flags of
// plugin://<source>.
func pluginDescribe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plugin describe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	search := fs.String("search", "", "only the settings whose name contains this text")
	// The source may come before the flags.
	var source string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		source, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if source == "" && fs.NArg() == 1 {
		source = fs.Arg(0)
	} else if source == "" || fs.NArg() > 0 {
		fmt.Fprint(stderr, pluginUsage)
		return 2
	}
	source = strings.TrimPrefix(source, "plugin://")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pl, builder, err := build.FindPlugin(ctx, source)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	settings, err := packer.Settings(pl, builder)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SETTING\tTYPE\tNOTE")
	shown := 0
	for _, s := range settings {
		// Packer's core settings, which opendango sets.
		if strings.HasPrefix(s.Name, "packer_") || !strings.Contains(s.Name, strings.ToLower(*search)) {
			continue
		}
		note := ""
		if by := packer.Reserved(source, s.Name); by != "" {
			note = "set by " + by
		}
		fmt.Fprintf(w, "--%s\t%s\t%s\n", s.Name, s.Type, note)
		shown++
	}
	w.Flush()
	fmt.Fprintf(stdout, "\n%d settings of plugin://%s (%s %s)\n", shown, source, pl.Name, pl.Version)
	if url := packer.DocURL(pl); url != "" {
		fmt.Fprintf(stdout, "What they do: %s\n", url)
	}
	return 0
}

// cmdInit writes a dango.yaml with every setting of the given plugin://
// sources (or of the Dangofile's), commented out with its type: an unset
// setting keeps the plugin's default, an empty one would not.
func cmdInit(args []string, stdout, stderr io.Writer) int {
	fs, c := newFlagSet("init", stderr)
	force := fs.Bool("force", false, "overwrite an existing file")
	// The sources may come before the flags.
	var sources []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sources, args = append(sources, args[0]), args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	sources = append(sources, fs.Args()...)
	path := c.config
	if path == "" {
		path = filepath.Join(filepath.Dir(c.file), plan.SettingsFile)
	}
	if _, err := os.Stat(path); err == nil && !*force {
		fmt.Fprintf(stderr, "opendango init: %s exists (--force overwrites it)\n", path)
		return 1
	}
	if len(sources) == 0 {
		// The plugin:// sources of the Dangofile.
		p, err := loadPlan(c)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		for _, st := range p.Stages {
			if src := st.From.Source; st.Parent == nil && st.IsPacker() && !slices.Contains(sources, src) {
				sources = append(sources, src)
			}
		}
		if len(sources) == 0 {
			fmt.Fprintf(stderr, "opendango init: %s has no plugin:// stage: give the sources (opendango init qemu)\n", c.file)
			return 1
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var b strings.Builder
	b.WriteString("# Settings of the plugin:// builders, by source type: the FROM flags of\n")
	b.WriteString("# their stages, which take precedence over this file. Uncomment what you\n")
	b.WriteString("# need: a setting left out keeps the plugin's default. ${VAR} is replaced\n")
	b.WriteString("# with the global ARGs. No secrets here: use ARG and --build-arg. The dango\n")
	b.WriteString("# key holds opendango's options.\n")
	for _, source := range sources {
		source = strings.TrimPrefix(source, "plugin://")
		pl, builder, err := build.FindPlugin(ctx, source)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		settings, err := packer.Settings(pl, builder)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(&b, "\n%s:\n", source)
		b.WriteString("  # dango:                    # opendango's options, not the plugin's\n")
		b.WriteString("  #   connect: ssh --user=debian --timeout=10m   # CONNECT of the stages without one\n")
		b.WriteString("  #   parent:                  # settings of a stage that starts from another one (FROM <stage>)\n")
		b.WriteString("  #     setting: ${DANGO_PARENT_ID}             # or ${DANGO_PARENT_FILE}: the parent's artifact\n")
		if url := packer.DocURL(pl); url != "" {
			fmt.Fprintf(&b, "  # What the settings do: %s\n", url)
		}
		width := 0
		var lines [][2]string
		for _, s := range settings {
			// Set by opendango or an instruction, or a secret.
			if strings.HasPrefix(s.Name, "packer_") || packer.Reserved(source, s.Name) != "" || packer.Sensitive(s.Name) {
				continue
			}
			line := fmt.Sprintf("# %s: %s", s.Name, placeholder(s.Type))
			width = max(width, len(line))
			lines = append(lines, [2]string{line, s.Type})
		}
		for _, l := range lines {
			fmt.Fprintf(&b, "  %-*s  # %s\n", width, l[0], l[1])
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "Wrote %s (%s)\n", path, strings.Join(sources, ", "))
	return 0
}

// placeholder is an empty value of a setting type, in YAML.
func placeholder(typ string) string {
	switch {
	case typ == "string":
		return `""`
	case typ == "number":
		return "0"
	case typ == "bool":
		return "false"
	case strings.HasPrefix(typ, "list"), strings.HasPrefix(typ, "set"):
		return "[]"
	default:
		return "{}"
	}
}
