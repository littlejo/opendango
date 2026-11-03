// Command opendango builds VM images described by a Dangofile.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/term"

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
  version    Print the version

Run 'opendango <command> -h' for the flags of a command.
`

func main() {
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
	buildArgs buildArgs
}

func newFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *commonFlags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := &commonFlags{buildArgs: buildArgs{}}
	fs.StringVar(&c.file, "f", "Dangofile", "path to the Dangofile")
	fs.Var(c.buildArgs, "build-arg", "set a build variable, KEY=VALUE (repeatable)")
	return fs, c
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
	return plan.Build(c.file, res, c.buildArgs)
}

func cmdValidate(args []string, stdout, stderr io.Writer) int {
	fs, c := newFlagSet("validate", stderr)
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
	fmt.Fprintf(stdout, "%s is valid\n", c.file)
	return 0
}

func cmdBuild(args []string, stdout, stderr io.Writer) int {
	fs, c := newFlagSet("build", stderr)
	onError := fs.String("on-error", "abort", "on failure: abort (destroy the VM and the intermediate templates), keep (leave them for debugging) or ask (retry, clean up or keep, from a terminal)")
	target := fs.String("target", "", "build only this stage and the stages it starts from")
	jobs := fs.Int("j", 1, "number of stages built at the same time")
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
