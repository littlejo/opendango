// Package instructions turns parsed Dangofile commands into typed
// instructions and validates them.
//
// Variable expansion is done by the caller through an Expander, because it
// depends on the ARG and ENV values in scope (see package plan).
package instructions

import (
	"errors"
	"fmt"
	"net"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/packer-plugin-sdk/bootcommand"

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

// From starts a stage, either from the infrastructure
// (FROM <builder>://<source> [AS name]) or from the image produced by an
// earlier stage (FROM <stage> [AS name]).
type From struct {
	Pos
	// Builder and Source are set for FROM <builder>://<source>.
	Builder string
	Source  string
	// Stage is set for FROM <stage>.
	Stage string
	Name  string
	Flags map[string]string
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
	// Exec is true for the exec (JSON) form, which SHELL does not change.
	Exec bool
}

// Label attaches metadata to the image: tags key=value on the template of
// OUTPUT --as=template.
type Label struct {
	Pos
	Labels []KeyValue
}

// Shell sets the shell that runs the shell form of RUN: the script is its
// last argument, like in Docker.
type Shell struct {
	Pos
	Args []string
}

// Copy uploads files from the build context into the VM.
type Copy struct {
	Pos
	Sources []string
	Dest    string
	Chmod   string
	Chown   string
	// From is the stage the sources come from (COPY --from=<stage>), "" for
	// the Dangofile directory. Its sources are absolute paths in that
	// stage's machine.
	From string
}

// Add is COPY plus http(s) URL sources, downloaded by opendango.
type Add struct {
	Copy
	// Checksum is the expected digest of the URL source (algo:hex), or "".
	Checksum string
}

// Reboot reboots the VM and waits for the connection to come back.
type Reboot struct {
	Pos
}

// Breakpoint pauses the build until the user presses Enter.
type Breakpoint struct {
	Pos
	// Message is shown with the pause (optional).
	Message string
}

// Serve serves a directory over HTTP while the stage builds.
type Serve struct {
	Pos
	// Dir is relative to the Dangofile directory.
	Dir string
	// Port is 0 for a free port chosen at build time.
	Port int
	// Addr is the address the VM reaches the server at; detected at build
	// time when empty.
	Addr string
}

// ServeAddrVar is the variable that holds the server address (ip:port).
const ServeAddrVar = "DANGO_HTTP_ADDR"

// ServeAddrMarker stands for ${DANGO_HTTP_ADDR} in a BOOT text: the address
// is only known at build time.
const ServeAddrMarker = "\x00DANGO_HTTP_ADDR\x00"

// Boot types keystrokes into the VM console, before the connection, with
// the Packer boot_command syntax (<enter>, <wait5>, <leftCtrlOn>...).
type Boot struct {
	Pos
	// Keys is the expanded text; ServeAddrMarker stands for the SERVE
	// address.
	Keys string
}

// Output kinds.
const (
	OutputTemplate = "template"
	OutputXVA      = "xva"
	// The post-processors: they run once the artifact is made.
	OutputManifest = "manifest"
	OutputChecksum = "checksum"
	OutputArchive  = "compress"
	OutputPlugin   = "plugin"
)

// ChecksumTypes are the hashes of OUTPUT --format=checksum.
var ChecksumTypes = []string{"md5", "sha1", "sha224", "sha256", "sha384", "sha512"}

// ArchiveExtensions are the archive formats of OUTPUT --format=compress, by
// the extension of the path.
var ArchiveExtensions = []string{".tar", ".tar.gz", ".tgz", ".zip"}

// Output produces an artifact once all other instructions have run.
type Output struct {
	Pos
	// Kind is one of the Output* constants.
	Kind string
	// Name is the template name (template only, optional).
	Name string
	// Path is the destination file (xva, manifest, checksum, compress).
	Path string
	// Compress is "", "zstd" or "gzip" (xva only).
	Compress string
	// Type is the hash of a checksum (one of ChecksumTypes).
	Type string
	// Source is the plugin's post-processor type (plugin only), such as
	// vagrant or docker-import; Settings are its flags.
	Source   string
	Settings map[string]string
}

// PostProcess tells whether an output runs once the artifact is made
// (a post-processor), rather than making it.
func (o *Output) PostProcess() bool {
	return o.Kind != OutputTemplate && o.Kind != OutputXVA
}

// Forbidden lists the Dockerfile instructions that make no sense for a VM.
var Forbidden = map[string]bool{
	"CMD": true, "ENTRYPOINT": true, "EXPOSE": true, "HEALTHCHECK": true,
	"ONBUILD": true, "STOPSIGNAL": true, "VOLUME": true,
}

// NotYetSupported lists the instructions planned but not implemented yet.
var NotYetSupported = map[string]bool{}

var (
	varName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	userName  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	stageName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]*$`)
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
	from := &From{Pos: pos(cmd), Flags: map[string]string{}}
	if builder, source, ok := strings.Cut(ref, "://"); ok {
		if builder == "" {
			return nil, fmt.Errorf("FROM %q: missing builder name before ://", ref)
		}
		from.Builder, from.Source = builder, source
	} else {
		if !stageName.MatchString(ref) {
			return nil, fmt.Errorf("FROM %q: expected <builder>://<source> or the name of an earlier stage", ref)
		}
		from.Stage = ref
	}
	if len(cmd.Args) == 3 {
		from.Name = cmd.Args[2]
		if !stageName.MatchString(from.Name) {
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
		run.Exec = true
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

// LabelReservedPrefix starts the cache tags: no label key may start with it.
const LabelReservedPrefix = "opendango-cache"

// ParseLabel parses `LABEL key=value ...` and the legacy `LABEL key value`.
// Keys and values are expanded, like in Docker.
func ParseLabel(cmd *parser.Command, expand Expander) (*Label, error) {
	if err := noFlags(cmd); err != nil {
		return nil, err
	}
	// BuildKit gives (key, value, separator) triplets, as for ENV.
	if len(cmd.Args) == 0 || len(cmd.Args)%3 != 0 {
		return nil, fmt.Errorf("LABEL requires at least one key=value")
	}
	l := &Label{Pos: pos(cmd)}
	for i := 0; i < len(cmd.Args); i += 3 {
		key, err := expand(cmd.Args[i])
		if err != nil {
			return nil, fmt.Errorf("LABEL: %w", err)
		}
		switch {
		case key == "":
			return nil, fmt.Errorf("LABEL: empty key")
		case strings.HasPrefix(key, LabelReservedPrefix):
			return nil, fmt.Errorf("LABEL %s: keys starting with %s are reserved for the cache", key, LabelReservedPrefix)
		case strings.ContainsAny(key, "=\n"):
			return nil, fmt.Errorf("LABEL %q: a key cannot contain = or a newline", key)
		}
		value, err := expand(cmd.Args[i+1])
		if err != nil {
			return nil, fmt.Errorf("LABEL %s: %w", key, err)
		}
		if strings.Contains(value, "\n") {
			return nil, fmt.Errorf("LABEL %s: a value cannot contain a newline", key)
		}
		l.Labels = append(l.Labels, KeyValue{Key: key, Value: value})
	}
	return l, nil
}

// ParseShell parses `SHELL ["executable", "parameters"...]`. Like in
// Docker, only the JSON form is accepted and the arguments are not expanded.
func ParseShell(cmd *parser.Command) (*Shell, error) {
	if err := noFlags(cmd); err != nil {
		return nil, err
	}
	if !cmd.JSON {
		return nil, fmt.Errorf(`SHELL requires the JSON form: SHELL ["executable", "parameters"]`)
	}
	if len(cmd.Args) == 0 || cmd.Args[0] == "" {
		return nil, fmt.Errorf("SHELL requires at least the executable")
	}
	return &Shell{Pos: pos(cmd), Args: cmd.Args}, nil
}

// ParseCopy parses `COPY [--chmod=MODE] [--chown=USER[:GROUP]] <src>... <dest>`.
func ParseCopy(cmd *parser.Command, expand Expander) (*Copy, error) {
	c, flags, err := parseCopy(cmd, expand, map[string]bool{"chmod": true, "chown": true, "from": true})
	if err != nil {
		return nil, err
	}
	from, ok := flags["from"]
	if !ok {
		return c, nil
	}
	if from == "" {
		return nil, fmt.Errorf("COPY: --from needs the name of a stage")
	}
	c.From = from
	// Paths in the source stage's machine, from its root, like Docker.
	for i, src := range c.Sources {
		if strings.ContainsAny(src, "*?[") {
			return nil, fmt.Errorf("COPY --from: %s: patterns are not supported, give the paths", src)
		}
		c.Sources[i] = path.Clean("/" + src)
		if c.Sources[i] == "/" {
			return nil, fmt.Errorf("COPY --from: the whole machine (/) cannot be copied")
		}
	}
	return c, nil
}

// checksumLen is the length in hex of the digests ADD --checksum accepts.
var checksumLen = map[string]int{"sha256": 64, "sha384": 96, "sha512": 128}

// ParseAdd parses `ADD [--chmod=MODE] [--chown=USER[:GROUP]]
// [--checksum=ALGO:HEX] <src>... <dest>`: COPY plus http(s) URL sources.
// --checksum needs a single URL source, like in Docker.
func ParseAdd(cmd *parser.Command, expand Expander) (*Add, error) {
	c, flags, err := parseCopy(cmd, expand, map[string]bool{"chmod": true, "chown": true, "checksum": true})
	if err != nil {
		return nil, err
	}
	a := &Add{Copy: *c, Checksum: flags["checksum"]}
	for _, src := range a.Sources {
		if strings.HasPrefix(src, "git@") || strings.HasPrefix(src, "git://") || (IsURL(src) && (strings.HasSuffix(src, ".git") || strings.Contains(src, ".git#"))) {
			return nil, fmt.Errorf("ADD %s: git sources are not supported yet", src)
		}
	}
	if _, ok := flags["checksum"]; ok {
		if len(a.Sources) != 1 || !IsURL(a.Sources[0]) {
			return nil, fmt.Errorf("ADD: --checksum requires exactly one http(s) URL source")
		}
		algo, sum, _ := strings.Cut(a.Checksum, ":")
		if n, ok := checksumLen[algo]; !ok || len(sum) != n || strings.Trim(sum, "0123456789abcdef") != "" {
			return nil, fmt.Errorf("ADD: invalid --checksum %q (expected sha256:<hex>, sha384:<hex> or sha512:<hex>)", a.Checksum)
		}
	}
	return a, nil
}

// IsURL tells whether an ADD source is an http(s) URL.
func IsURL(src string) bool {
	return strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://")
}

// parseCopy parses the arguments and flags shared by COPY and ADD.
func parseCopy(cmd *parser.Command, expand Expander, allowed map[string]bool) (*Copy, map[string]string, error) {
	kw := cmd.Keyword
	flags, err := parseFlags(cmd, expand, allowed)
	if err != nil {
		return nil, nil, err
	}
	if len(cmd.Heredocs) > 0 {
		return nil, nil, fmt.Errorf("%s with a heredoc is not supported yet", kw)
	}
	if cmd.JSON {
		return nil, nil, fmt.Errorf("%s: the JSON form is not supported yet", kw)
	}
	args, err := expandAll(cmd.Args, expand)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", kw, err)
	}
	if len(args) < 2 {
		return nil, nil, fmt.Errorf("%s requires at least one source and a destination", kw)
	}
	c := &Copy{Pos: pos(cmd), Sources: args[:len(args)-1], Dest: args[len(args)-1], Chmod: flags["chmod"], Chown: flags["chown"]}
	if c.Chmod != "" && !regexp.MustCompile(`^[0-7]{3,4}$`).MatchString(c.Chmod) {
		return nil, nil, fmt.Errorf("%s: invalid --chmod %q (expected an octal mode such as 0644)", kw, c.Chmod)
	}
	if _, ok := flags["chown"]; ok && !regexp.MustCompile(`^[A-Za-z0-9_.-]+(:[A-Za-z0-9_.-]+)?$`).MatchString(c.Chown) {
		return nil, nil, fmt.Errorf("%s: invalid --chown %q (expected user or user:group)", kw, c.Chown)
	}
	return c, flags, nil
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

// ParseBreakpoint parses `BREAKPOINT [message]`. The words of the message
// are joined with a space; an empty variable leaves no trailing space.
func ParseBreakpoint(cmd *parser.Command, expand Expander) (*Breakpoint, error) {
	if err := noFlags(cmd); err != nil {
		return nil, err
	}
	words, err := expandAll(cmd.Args, expand)
	if err != nil {
		return nil, fmt.Errorf("BREAKPOINT: %w", err)
	}
	return &Breakpoint{Pos: pos(cmd), Message: strings.TrimSpace(strings.Join(words, " "))}, nil
}

// ParseServe parses `SERVE [--port=N] [--addr=IP] <dir>`. The directory must
// stay inside the Dangofile directory.
func ParseServe(cmd *parser.Command, expand Expander) (*Serve, error) {
	flags, err := parseFlags(cmd, expand, map[string]bool{"port": true, "addr": true})
	if err != nil {
		return nil, err
	}
	args, err := expandAll(cmd.Args, expand)
	if err != nil {
		return nil, fmt.Errorf("SERVE: %w", err)
	}
	if len(args) != 1 || args[0] == "" {
		return nil, fmt.Errorf("SERVE requires exactly one directory")
	}
	s := &Serve{Pos: pos(cmd), Dir: args[0], Addr: flags["addr"]}
	if !filepath.IsLocal(filepath.FromSlash(s.Dir)) {
		return nil, fmt.Errorf("SERVE: %q is outside the Dangofile directory", s.Dir)
	}
	if p, ok := flags["port"]; ok {
		s.Port, err = strconv.Atoi(p)
		if err != nil || s.Port < 1 || s.Port > 65535 {
			return nil, fmt.Errorf("SERVE: invalid --port %q", p)
		}
	}
	if _, ok := flags["addr"]; ok && net.ParseIP(s.Addr) == nil {
		return nil, fmt.Errorf("SERVE: invalid --addr %q (expected an IP address)", s.Addr)
	}
	return s, nil
}

// ParseBoot parses `BOOT <keys>`. The parser keeps the rest of the line as
// one argument (no flags); expand must leave the quotes in place. The keys
// are checked with the boot_command parser of packer-plugin-sdk.
func ParseBoot(cmd *parser.Command, expand Expander) (*Boot, error) {
	if err := noFlags(cmd); err != nil {
		return nil, err
	}
	if len(cmd.Args) != 1 {
		return nil, fmt.Errorf("BOOT requires the keys to type")
	}
	keys, err := expand(cmd.Args[0])
	if err != nil {
		return nil, fmt.Errorf("BOOT: %w", err)
	}
	seq, err := bootcommand.GenerateExpressionSequence(strings.ReplaceAll(keys, ServeAddrMarker, "192.0.2.1:8000"))
	if err != nil {
		return nil, fmt.Errorf("BOOT: %w", err)
	}
	if errs := seq.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("BOOT: %w", errors.Join(errs...))
	}
	return &Boot{Pos: pos(cmd), Keys: keys}, nil
}

// ParseOutput parses `OUTPUT --as=template [name]` and
// `OUTPUT --format=xva [--compress=zstd|gzip] <path>`.
func ParseOutput(cmd *parser.Command, expand Expander) (*Output, error) {
	args, err := expandAll(cmd.Args, expand)
	if err != nil {
		return nil, fmt.Errorf("OUTPUT: %w", err)
	}
	// OUTPUT [--settings] plugin://<post-processor>: the flags are the
	// post-processor's settings.
	if len(args) == 1 && strings.HasPrefix(args[0], "plugin://") {
		return parsePluginOutput(cmd, args[0], expand)
	}
	flags, err := parseFlags(cmd, expand, map[string]bool{"as": true, "format": true, "compress": true, "type": true})
	if err != nil {
		return nil, err
	}
	as, hasAs := flags["as"]
	format, hasFormat := flags["format"]
	compress, hasCompress := flags["compress"]
	typ, hasType := flags["type"]
	out := &Output{Pos: pos(cmd)}
	if hasType && format != OutputChecksum {
		return nil, fmt.Errorf("OUTPUT: --type is for --format=checksum")
	}
	if hasCompress && format != OutputXVA {
		return nil, fmt.Errorf("OUTPUT: --compress is for --format=xva")
	}
	switch {
	case hasAs == hasFormat:
		return nil, fmt.Errorf("OUTPUT requires exactly one of --as=template, --format=<format> or plugin://<post-processor>")
	case hasAs:
		if as != OutputTemplate {
			return nil, fmt.Errorf("OUTPUT: unknown --as %q (expected template)", as)
		}
		if len(args) > 1 {
			return nil, fmt.Errorf("OUTPUT --as=template takes at most one name")
		}
		out.Kind = OutputTemplate
		if len(args) == 1 {
			out.Name = args[0]
		}
		return out, nil
	}
	switch format {
	case OutputXVA, OutputManifest, OutputChecksum, OutputArchive:
	default:
		return nil, fmt.Errorf("OUTPUT: unknown --format %q (expected xva, manifest, checksum or compress)", format)
	}
	if len(args) != 1 {
		return nil, fmt.Errorf("OUTPUT --format=%s requires exactly one destination path", format)
	}
	out.Kind, out.Path = format, args[0]
	switch format {
	case OutputXVA:
		if compress != "" && compress != "zstd" && compress != "gzip" {
			return nil, fmt.Errorf("OUTPUT: unknown --compress %q (expected zstd or gzip)", compress)
		}
		out.Compress = compress
	case OutputChecksum:
		out.Type = "sha256"
		if hasType {
			out.Type = typ
		}
		if !slices.Contains(ChecksumTypes, out.Type) {
			return nil, fmt.Errorf("OUTPUT: unknown --type %q (expected %s)", out.Type, strings.Join(ChecksumTypes, ", "))
		}
	case OutputArchive:
		if !slices.ContainsFunc(ArchiveExtensions, func(ext string) bool { return strings.HasSuffix(out.Path, ext) }) {
			return nil, fmt.Errorf("OUTPUT --format=compress: %s: the extension gives the format (%s)", out.Path, strings.Join(ArchiveExtensions, ", "))
		}
	}
	return out, nil
}

// parsePluginOutput parses OUTPUT [--settings] plugin://<post-processor>.
func parsePluginOutput(cmd *parser.Command, source string, expand Expander) (*Output, error) {
	out := &Output{Pos: pos(cmd), Kind: OutputPlugin, Source: strings.TrimPrefix(source, "plugin://"), Settings: map[string]string{}}
	if out.Source == "" {
		return nil, fmt.Errorf("OUTPUT: missing post-processor after plugin://")
	}
	for _, f := range cmd.Flags {
		name, value, hasValue := strings.Cut(strings.TrimPrefix(f, "--"), "=")
		if !hasValue {
			return nil, fmt.Errorf("OUTPUT: flag --%s requires a value", name)
		}
		if _, dup := out.Settings[name]; dup {
			return nil, fmt.Errorf("OUTPUT: flag --%s set twice", name)
		}
		v, err := expand(value)
		if err != nil {
			return nil, fmt.Errorf("OUTPUT: flag --%s: %w", name, err)
		}
		out.Settings[name] = v
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
