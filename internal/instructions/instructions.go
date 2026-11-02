// Package instructions turns parsed Dangofile commands into typed
// instructions and validates them.
//
// Variable expansion is done by the caller through an Expander, because it
// depends on the ARG and ENV values in scope (see package plan).
package instructions

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/littlejo/opendango/internal/parser"
)

// Expander expands ${VAR} references in a word and removes its quotes.
type Expander func(word string) (string, error)

// Instruction is any typed instruction.
type Instruction interface {
	Line() int
}

// Pos is the location of an instruction in the Dangofile.
type Pos struct {
	StartLine int
}

func (p Pos) Line() int { return p.StartLine }

// KeyValue is a variable assignment, in declaration order.
type KeyValue struct {
	Key   string
	Value string
}

// Arg declares a build variable.
type Arg struct {
	Pos
	Name    string
	Default string
	// HasDefault is false for `ARG NAME`.
	HasDefault bool
	// Value and Set are resolved by the plan from --build-arg, the default
	// and the global ARG of the same name. Set is false when none applies.
	Value string
	Set   bool
}

// From selects the builder and the source: FROM <builder>://<source> [AS name].
type From struct {
	Pos
	Builder string
	Source  string
	Name    string
	Flags   map[string]string
}

// Connect describes how to reach the VM.
type Connect struct {
	Pos
	Protocol string
	User     string
	Port     int
	Timeout  time.Duration
}

// Env sets variables for the following instructions.
type Env struct {
	Pos
	Vars    []KeyValue
	Persist bool
}

// User sets the user the following RUN and COPY run as.
type User struct {
	Pos
	Name string
}

// Workdir sets the working directory of the following RUN and COPY.
type Workdir struct {
	Pos
	Path string
}

// Run executes a shell script in the VM. Script is not expanded: the remote
// shell does it, with the ARG and ENV values exported.
type Run struct {
	Pos
	Script string
}

// Copy uploads files from the build context into the VM.
type Copy struct {
	Pos
	Sources []string
	Dest    string
	Chmod   string
	Chown   string
}

// Reboot reboots the VM and waits for the connection to come back.
type Reboot struct {
	Pos
}

// Output kinds.
const (
	OutputTemplate = "template"
	OutputXVA      = "xva"
)

// Output produces an artifact once all other instructions have run.
type Output struct {
	Pos
	// Kind is OutputTemplate or OutputXVA.
	Kind string
	// Name is the template name (template only, optional).
	Name string
	// Path is the destination file (xva only).
	Path string
	// Compress is "", "zstd" or "gzip" (xva only).
	Compress string
}

// Forbidden lists the Dockerfile instructions that make no sense for a VM.
var Forbidden = map[string]bool{
	"CMD": true, "ENTRYPOINT": true, "EXPOSE": true, "HEALTHCHECK": true,
	"ONBUILD": true, "STOPSIGNAL": true, "VOLUME": true,
}

// NotYetSupported lists the instructions planned but not implemented yet.
var NotYetSupported = map[string]bool{
	"ADD": true, "BOOT": true, "BREAKPOINT": true, "LABEL": true,
	"SERVE": true, "SHELL": true,
}

var (
	varName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	userName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
)

// ParseArg parses `ARG NAME[=default] ...`, one Arg per declared variable.
// Defaults are expanded with the variables declared before.
func ParseArg(cmd *parser.Command, expand Expander) ([]*Arg, error) {
	if err := noFlags(cmd); err != nil {
		return nil, err
	}
	if len(cmd.Args) == 0 {
		return nil, fmt.Errorf("ARG requires at least one argument")
	}
	var out []*Arg
	for _, a := range cmd.Args {
		name, def, hasDefault := strings.Cut(a, "=")
		if !varName.MatchString(name) {
			return nil, fmt.Errorf("ARG: invalid variable name %q", name)
		}
		arg := &Arg{Pos: pos(cmd), Name: name, HasDefault: hasDefault}
		if hasDefault {
			v, err := expand(def)
			if err != nil {
				return nil, fmt.Errorf("ARG %s: %w", name, err)
			}
			arg.Default = v
		}
		out = append(out, arg)
	}
	return out, nil
}

// ParseFrom parses `FROM [--flag=value...] <builder>://<source> [AS name]`.
// Flags are not checked here: they belong to the builder.
func ParseFrom(cmd *parser.Command, expand Expander) (*From, error) {
	if len(cmd.Args) != 1 && !(len(cmd.Args) == 3 && strings.EqualFold(cmd.Args[1], "AS")) {
		return nil, fmt.Errorf("FROM requires exactly one source, optionally followed by AS <name>")
	}
	ref, err := expand(cmd.Args[0])
	if err != nil {
		return nil, fmt.Errorf("FROM: %w", err)
	}
	builder, source, ok := strings.Cut(ref, "://")
	if !ok {
		return nil, fmt.Errorf("FROM %q: expected <builder>://<source> (starting from another stage is not supported yet)", ref)
	}
	if builder == "" {
		return nil, fmt.Errorf("FROM %q: missing builder name before ://", ref)
	}
	from := &From{Pos: pos(cmd), Builder: builder, Source: source, Flags: map[string]string{}}
	if len(cmd.Args) == 3 {
		from.Name = cmd.Args[2]
		if !regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]*$`).MatchString(from.Name) {
			return nil, fmt.Errorf("FROM: invalid stage name %q", from.Name)
		}
	}
	for _, f := range cmd.Flags {
		name, value, ok := strings.Cut(strings.TrimPrefix(f, "--"), "=")
		if !ok {
			return nil, fmt.Errorf("FROM: flag --%s requires a value", name)
		}
		if _, dup := from.Flags[name]; dup {
			return nil, fmt.Errorf("FROM: flag --%s set twice", name)
		}
		v, err := expand(value)
		if err != nil {
			return nil, fmt.Errorf("FROM: flag --%s: %w", name, err)
		}
		from.Flags[name] = v
	}
	return from, nil
}

// ParseConnect parses `CONNECT ssh --user=<user> [--port=22] [--timeout=10m]`.
func ParseConnect(cmd *parser.Command, expand Expander) (*Connect, error) {
	flags, err := parseFlags(cmd, expand, map[string]bool{"user": true, "port": true, "timeout": true})
	if err != nil {
		return nil, err
	}
	args, err := expandAll(cmd.Args, expand)
	if err != nil {
		return nil, fmt.Errorf("CONNECT: %w", err)
	}
	if len(args) != 1 {
		return nil, fmt.Errorf("CONNECT requires exactly one protocol (ssh)")
	}
	c := &Connect{Pos: pos(cmd), Protocol: args[0], User: flags["user"], Port: 22, Timeout: 10 * time.Minute}
	switch c.Protocol {
	case "ssh":
	case "winrm":
		return nil, fmt.Errorf("CONNECT winrm is not supported yet")
	default:
		return nil, fmt.Errorf("CONNECT: unknown protocol %q (expected ssh)", c.Protocol)
	}
	if c.User == "" {
		return nil, fmt.Errorf("CONNECT requires --user=<user>")
	}
	if p, ok := flags["port"]; ok {
		c.Port, err = strconv.Atoi(p)
		if err != nil || c.Port < 1 || c.Port > 65535 {
			return nil, fmt.Errorf("CONNECT: invalid --port %q", p)
		}
	}
	if t, ok := flags["timeout"]; ok {
		c.Timeout, err = time.ParseDuration(t)
		if err != nil || c.Timeout <= 0 {
			return nil, fmt.Errorf("CONNECT: invalid --timeout %q", t)
		}
	}
	return c, nil
}

// ParseEnv parses `ENV [--persist] KEY=value ...` and the legacy `ENV KEY value`.
func ParseEnv(cmd *parser.Command, expand Expander) (*Env, error) {
	flags, err := parseFlags(cmd, expand, map[string]bool{"persist": false})
	if err != nil {
		return nil, err
	}
	_, persist := flags["persist"]
	env := &Env{Pos: pos(cmd), Persist: persist}
	// BuildKit gives (key, value, separator) triplets; the separator is "="
	// for KEY=value and "" for the legacy form.
	if len(cmd.Args) == 0 || len(cmd.Args)%3 != 0 {
		return nil, fmt.Errorf("ENV requires at least one KEY=value")
	}
	for i := 0; i < len(cmd.Args); i += 3 {
		key := cmd.Args[i]
		if !varName.MatchString(key) {
			return nil, fmt.Errorf("ENV: invalid variable name %q", key)
		}
		value, err := expand(cmd.Args[i+1])
		if err != nil {
			return nil, fmt.Errorf("ENV %s: %w", key, err)
		}
		if persist && strings.ContainsAny(value, "\"\n") {
			return nil, fmt.Errorf("ENV --persist %s: value cannot contain a double quote or a newline", key)
		}
		env.Vars = append(env.Vars, KeyValue{Key: key, Value: value})
	}
	return env, nil
}

// ParseUser parses `USER <name>`.
func ParseUser(cmd *parser.Command, expand Expander) (*User, error) {
	name, err := singleArg(cmd, expand)
	if err != nil {
		return nil, err
	}
	if strings.Contains(name, ":") {
		return nil, fmt.Errorf("USER: groups are not supported, use a user name")
	}
	if !userName.MatchString(name) {
		return nil, fmt.Errorf("USER: invalid user name %q", name)
	}
	return &User{Pos: pos(cmd), Name: name}, nil
}

// ParseWorkdir parses `WORKDIR <path>`.
func ParseWorkdir(cmd *parser.Command, expand Expander) (*Workdir, error) {
	p, err := singleArg(cmd, expand)
	if err != nil {
		return nil, err
	}
	return &Workdir{Pos: pos(cmd), Path: p}, nil
}

// ParseRun parses the shell, exec (JSON) and heredoc forms of RUN.
func ParseRun(cmd *parser.Command) (*Run, error) {
	if err := noFlags(cmd); err != nil {
		return nil, err
	}
	if len(cmd.Args) == 0 {
		return nil, fmt.Errorf("RUN requires a command")
	}
	run := &Run{Pos: pos(cmd)}
	switch {
	case cmd.JSON:
		quoted := make([]string, len(cmd.Args))
		for i, a := range cmd.Args {
			quoted[i] = ShellQuote(a)
		}
		run.Script = "exec " + strings.Join(quoted, " ")
	case len(cmd.Heredocs) > 0 && onlyHeredocMarkers(cmd.Args[0]):
		// RUN <<EOF: the heredoc content is the script itself.
		for _, h := range cmd.Heredocs {
			run.Script += h.Content
		}
	case len(cmd.Heredocs) > 0:
		// RUN cmd <<EOF: let the remote shell feed the heredoc to cmd.
		var b strings.Builder
		b.WriteString(cmd.Args[0])
		b.WriteString("\n")
		for _, h := range cmd.Heredocs {
			b.WriteString(h.Content)
			b.WriteString(h.Name)
			b.WriteString("\n")
		}
		run.Script = b.String()
	default:
		run.Script = cmd.Args[0]
	}
	return run, nil
}

// ParseCopy parses `COPY [--chmod=MODE] [--chown=USER[:GROUP]] <src>... <dest>`.
func ParseCopy(cmd *parser.Command, expand Expander) (*Copy, error) {
	flags, err := parseFlags(cmd, expand, map[string]bool{"chmod": true, "chown": true})
	if err != nil {
		return nil, err
	}
	if len(cmd.Heredocs) > 0 {
		return nil, fmt.Errorf("COPY with a heredoc is not supported yet")
	}
	if cmd.JSON {
		return nil, fmt.Errorf("COPY: the JSON form is not supported yet")
	}
	args, err := expandAll(cmd.Args, expand)
	if err != nil {
		return nil, fmt.Errorf("COPY: %w", err)
	}
	if len(args) < 2 {
		return nil, fmt.Errorf("COPY requires at least one source and a destination")
	}
	c := &Copy{Pos: pos(cmd), Sources: args[:len(args)-1], Dest: args[len(args)-1], Chmod: flags["chmod"], Chown: flags["chown"]}
	if c.Chmod != "" && !regexp.MustCompile(`^[0-7]{3,4}$`).MatchString(c.Chmod) {
		return nil, fmt.Errorf("COPY: invalid --chmod %q (expected an octal mode such as 0644)", c.Chmod)
	}
	if _, ok := flags["chown"]; ok && !regexp.MustCompile(`^[A-Za-z0-9_.-]+(:[A-Za-z0-9_.-]+)?$`).MatchString(c.Chown) {
		return nil, fmt.Errorf("COPY: invalid --chown %q (expected user or user:group)", c.Chown)
	}
	return c, nil
}

// ParseReboot parses `REBOOT`.
func ParseReboot(cmd *parser.Command) (*Reboot, error) {
	if err := noFlags(cmd); err != nil {
		return nil, err
	}
	if len(cmd.Args) != 0 {
		return nil, fmt.Errorf("REBOOT takes no argument")
	}
	return &Reboot{Pos: pos(cmd)}, nil
}

// ParseOutput parses `OUTPUT --as=template [name]` and
// `OUTPUT --format=xva [--compress=zstd|gzip] <path>`.
func ParseOutput(cmd *parser.Command, expand Expander) (*Output, error) {
	flags, err := parseFlags(cmd, expand, map[string]bool{"as": true, "format": true, "compress": true})
	if err != nil {
		return nil, err
	}
	args, err := expandAll(cmd.Args, expand)
	if err != nil {
		return nil, fmt.Errorf("OUTPUT: %w", err)
	}
	as, hasAs := flags["as"]
	format, hasFormat := flags["format"]
	compress, hasCompress := flags["compress"]
	out := &Output{Pos: pos(cmd)}
	switch {
	case hasAs == hasFormat:
		return nil, fmt.Errorf("OUTPUT requires exactly one of --as=template or --format=xva")
	case hasAs:
		if as != OutputTemplate {
			return nil, fmt.Errorf("OUTPUT: unknown --as %q (expected template)", as)
		}
		if hasCompress {
			return nil, fmt.Errorf("OUTPUT --as=template does not accept --compress")
		}
		if len(args) > 1 {
			return nil, fmt.Errorf("OUTPUT --as=template takes at most one name")
		}
		out.Kind = OutputTemplate
		if len(args) == 1 {
			out.Name = args[0]
		}
	default:
		if format != OutputXVA {
			return nil, fmt.Errorf("OUTPUT: unknown --format %q (expected xva)", format)
		}
		if len(args) != 1 {
			return nil, fmt.Errorf("OUTPUT --format=xva requires exactly one destination path")
		}
		if compress != "" && compress != "zstd" && compress != "gzip" {
			return nil, fmt.Errorf("OUTPUT: unknown --compress %q (expected zstd or gzip)", compress)
		}
		out.Kind, out.Path, out.Compress = OutputXVA, args[0], compress
	}
	return out, nil
}

// ShellQuote quotes s for a POSIX shell.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func pos(cmd *parser.Command) Pos { return Pos{StartLine: cmd.StartLine} }

func onlyHeredocMarkers(s string) bool {
	for _, w := range strings.Fields(s) {
		if !strings.HasPrefix(w, "<<") {
			return false
		}
	}
	return true
}

func noFlags(cmd *parser.Command) error {
	if len(cmd.Flags) > 0 {
		return fmt.Errorf("%s: unknown flag %s", cmd.Keyword, cmd.Flags[0])
	}
	return nil
}

func singleArg(cmd *parser.Command, expand Expander) (string, error) {
	if err := noFlags(cmd); err != nil {
		return "", err
	}
	if len(cmd.Args) != 1 {
		return "", fmt.Errorf("%s requires exactly one argument", cmd.Keyword)
	}
	v, err := expand(cmd.Args[0])
	if err != nil {
		return "", fmt.Errorf("%s: %w", cmd.Keyword, err)
	}
	if v == "" {
		return "", fmt.Errorf("%s: empty argument", cmd.Keyword)
	}
	return v, nil
}

func expandAll(words []string, expand Expander) ([]string, error) {
	out := make([]string, len(words))
	for i, w := range words {
		v, err := expand(w)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// parseFlags checks cmd's flags against allowed (name -> takes a value) and
// returns their expanded values. Boolean flags map to "".
func parseFlags(cmd *parser.Command, expand Expander, allowed map[string]bool) (map[string]string, error) {
	out := map[string]string{}
	for _, f := range cmd.Flags {
		name, value, hasValue := strings.Cut(strings.TrimPrefix(f, "--"), "=")
		takesValue, ok := allowed[name]
		switch {
		case !ok:
			return nil, fmt.Errorf("%s: unknown flag --%s", cmd.Keyword, name)
		case takesValue && !hasValue:
			return nil, fmt.Errorf("%s: flag --%s requires a value", cmd.Keyword, name)
		case !takesValue && hasValue:
			return nil, fmt.Errorf("%s: flag --%s takes no value", cmd.Keyword, name)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("%s: flag --%s set twice", cmd.Keyword, name)
		}
		v, err := expand(value)
		if err != nil {
			return nil, fmt.Errorf("%s: flag --%s: %w", cmd.Keyword, name, err)
		}
		out[name] = v
	}
	return out, nil
}
