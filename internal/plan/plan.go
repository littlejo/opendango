// Package plan turns a parsed Dangofile into a validated build plan: it
// resolves ARG values, expands variables and checks the stage-level rules.
package plan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/shell"

	"github.com/littlejo/opendango/builders/packer"
	"github.com/littlejo/opendango/builders/xo"
	"github.com/littlejo/opendango/internal/instructions"
	"github.com/littlejo/opendango/internal/parser"
)

// Plan is a validated Dangofile.
type Plan struct {
	File string
	// Stages are in file order: a stage always comes after its parent.
	Stages   []*Stage
	Warnings []string
}

// Stage is everything between a FROM and the next FROM (or the end of the
// file).
type Stage struct {
	From *instructions.From
	// Index is the position of the stage in the file, from 0.
	Index int
	// Parent is the stage this one starts from (FROM <stage>); nil for
	// FROM <builder>://<source>.
	Parent *Stage
	// Children are the stages that start from this one.
	Children []*Stage
	// Connect is the stage's CONNECT, or the one inherited from its parent;
	// nil when no instruction needs a connection.
	Connect *instructions.Connect
	// Steps run in order: *Arg, *Env, *User, *Shell, *Workdir, *Run, *Copy,
	// *Add, *Reboot, *Breakpoint.
	Steps []instructions.Instruction
	// Outputs run after all steps, once the VM is stopped.
	Outputs []*instructions.Output
	// Serve is the stage's SERVE, or nil. The server runs from before the
	// VM starts to the end of the stage.
	Serve *instructions.Serve
	// Boot are the stage's BOOT, typed in order once the VM has started,
	// before the connection.
	Boot []*instructions.Boot
	// Labels are the LABEL values of the stage and of its ancestors, in the
	// order they were first set: tags key=value on the OUTPUT template.
	Labels []instructions.KeyValue

	// ownConnect is true when the stage sets CONNECT itself.
	ownConnect bool
	// labelLine is the line of the stage's first LABEL, 0 without one.
	labelLine int
	// usesVM is true once an instruction of the stage needs the VM.
	usesVM bool
	// env holds the ENV values at the end of the stage, inherited by the
	// stages that start from it (ARG values are not, like in Docker).
	env map[string]string
}

// Name is the stage name (AS <name>), or stage-<index> when unnamed.
func (s *Stage) Name() string {
	if s.From.Name != "" {
		return s.From.Name
	}
	return fmt.Sprintf("stage-%d", s.Index)
}

// Root returns the first ancestor, the one that starts from a builder.
func (s *Stage) Root() *Stage {
	for s.Parent != nil {
		s = s.Parent
	}
	return s
}

// PackerFlags returns the FROM flags of a plugin:// stage: its own for a
// root stage; for a stage that starts from another one, also the flags it
// inherits (packer.Known.StageFlags).
func (s *Stage) PackerFlags() (map[string]string, error) {
	if s.Parent == nil {
		return s.From.Flags, nil
	}
	k := s.known()
	if k == nil || k.Source == "" {
		return nil, fmt.Errorf("FROM %s: a stage cannot start from a plugin://%s stage: its artifact is the plugin's (opendango knows the artifacts of plugin://%s)", s.From.Stage, s.Root().From.Source, packer.KnownSources())
	}
	parent, err := s.Parent.PackerFlags()
	if err != nil {
		return nil, err
	}
	flags, err := k.StageFlags(parent, s.From.Flags)
	if err != nil {
		return nil, fmt.Errorf("FROM %s: %w", s.From.Stage, err)
	}
	return flags, nil
}

// known returns what opendango knows of the Packer builder of the stage,
// nil for a native stage or a builder that is not in the table.
func (s *Stage) known() *packer.Known {
	if !s.IsPacker() {
		return nil
	}
	return packer.Lookup(s.Root().From.Source)
}

// IsPacker tells whether a Packer plugin builds the stage
// (FROM plugin://...).
func (s *Stage) IsPacker() bool { return s.Root().From.Builder == "plugin" }

// TemplateOutput returns the stage's OUTPUT --as=template, or nil.
func (s *Stage) TemplateOutput() *instructions.Output {
	for _, out := range s.Outputs {
		if out.Kind == instructions.OutputTemplate {
			return out
		}
	}
	return nil
}

// Select returns the stages to build, in order: every stage, or the target
// and its ancestors.
func (p *Plan) Select(target string) ([]*Stage, error) {
	if target == "" {
		return p.Stages, nil
	}
	var names []string
	for _, s := range p.Stages {
		if s.From.Name != target {
			if s.From.Name != "" {
				names = append(names, s.From.Name)
			}
			continue
		}
		var chain []*Stage
		for a := s; a != nil; a = a.Parent {
			chain = append([]*Stage{a}, chain...)
		}
		return chain, nil
	}
	return nil, fmt.Errorf("no stage named %q (stages: %v)", target, names)
}

// Error is a validation error located in the Dangofile.
type Error struct {
	File string
	Line int
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("%s:%d: %v", e.File, e.Line, e.Err) }
func (e *Error) Unwrap() error { return e.Err }

// Build validates res and returns its plan. buildArgs are the --build-arg
// values.
func Build(file string, res *parser.Result, buildArgs map[string]string) (*Plan, error) {
	p := &Plan{File: file}
	lex := shell.NewLex(res.EscapeToken)
	usedArgs := map[string]bool{}
	fail := func(line int, err error) (*Plan, error) {
		return nil, &Error{File: file, Line: line, Err: err}
	}

	// Variables in scope for expansion. Before FROM: the global ARGs. In the
	// stage: the stage ARGs, then ENV (Docker semantics).
	global := map[string]string{}
	scope := global
	var stage *Stage
	expand := func(word string) (string, error) {
		res, err := lex.ProcessWordWithMatches(word, envGetter(scope))
		if _, ok := res.Unmatched[instructions.ServeAddrVar]; ok && err == nil {
			err = unsetServeAddr()
		}
		return res.Result, err
	}
	// BOOT keeps its quotes: they are typed. ${DANGO_HTTP_ADDR} becomes a
	// marker, replaced at build time.
	bootLex := shell.NewLex(res.EscapeToken)
	bootLex.SkipProcessQuotes = true
	expandBoot := func(word string) (string, error) {
		env := envGetter{}
		for k, v := range scope {
			env[k] = v
		}
		if stage.Serve != nil {
			env[instructions.ServeAddrVar] = instructions.ServeAddrMarker
		}
		res, err := bootLex.ProcessWordWithMatches(word, env)
		if _, ok := res.Unmatched[instructions.ServeAddrVar]; ok && err == nil {
			err = unsetServeAddr()
		}
		return res.Result, err
	}

	byName := map[string]*Stage{}
	for _, cmd := range res.Commands {
		line := cmd.StartLine
		switch {
		case instructions.Forbidden[cmd.Keyword]:
			return fail(line, fmt.Errorf("%s has no meaning for a VM image", cmd.Keyword))
		case instructions.NotYetSupported[cmd.Keyword]:
			return fail(line, fmt.Errorf("%s is not supported yet", cmd.Keyword))
		}

		if stage == nil && cmd.Keyword != "ARG" && cmd.Keyword != "FROM" {
			return fail(line, fmt.Errorf("%s before FROM: only ARG can come before the first FROM", cmd.Keyword))
		}

		switch cmd.Keyword {
		case "ARG":
			args, err := instructions.ParseArg(cmd, expand)
			if err != nil {
				return fail(line, err)
			}
			for _, a := range args {
				if a.Name == instructions.ServeAddrVar {
					return fail(line, reserved(a.Name))
				}
				resolveArg(a, buildArgs, global, stage == nil)
				usedArgs[a.Name] = true
				if a.Set {
					scope[a.Name] = a.Value
				}
				if stage != nil {
					stage.Steps = append(stage.Steps, a)
				}
			}

		case "FROM":
			// FROM lines only see the global ARGs.
			scope = global
			from, err := instructions.ParseFrom(literalFlags(cmd, res.EscapeToken), expand)
			if err != nil {
				return fail(line, err)
			}
			if prev, dup := byName[from.Name]; dup && from.Name != "" {
				return fail(line, fmt.Errorf("stage %q already defined at line %d", from.Name, prev.From.Line()))
			}
			stage = &Stage{From: from, Index: len(p.Stages), env: map[string]string{}}
			if from.Stage != "" {
				parent, ok := byName[from.Stage]
				if !ok {
					return fail(line, fmt.Errorf("FROM %s: no earlier stage named %q", from.Stage, from.Stage))
				}
				stage.Parent = parent
				parent.Children = append(parent.Children, stage)
				// Inherited from the parent, like in Docker (plus CONNECT).
				stage.Connect = parent.Connect
				stage.Labels = append([]instructions.KeyValue{}, parent.Labels...)
				for k, v := range parent.env {
					stage.env[k] = v
				}
			}
			if err := checkBuilder(stage); err != nil {
				return fail(line, err)
			}
			if from.Name != "" {
				byName[from.Name] = stage
			}
			p.Stages = append(p.Stages, stage)
			scope = map[string]string{}
			for k, v := range stage.env {
				scope[k] = v
			}

		case "CONNECT":
			if stage.ownConnect {
				return fail(line, fmt.Errorf("CONNECT already set at line %d", stage.Connect.Line()))
			}
			c, err := instructions.ParseConnect(cmd, expand)
			if err != nil {
				return fail(line, err)
			}
			stage.Connect, stage.ownConnect = c, true

		case "ENV":
			env, err := instructions.ParseEnv(cmd, expand)
			if err != nil {
				return fail(line, err)
			}
			if env.Persist && stage.Connect == nil {
				return fail(line, needsConnect("ENV --persist"))
			}
			stage.usesVM = stage.usesVM || env.Persist
			for _, kv := range env.Vars {
				if kv.Key == instructions.ServeAddrVar {
					return fail(line, reserved(kv.Key))
				}
				scope[kv.Key] = kv.Value
				stage.env[kv.Key] = kv.Value
			}
			stage.Steps = append(stage.Steps, env)

		case "USER":
			u, err := instructions.ParseUser(cmd, expand)
			if err != nil {
				return fail(line, err)
			}
			stage.Steps = append(stage.Steps, u)

		case "SHELL":
			sh, err := instructions.ParseShell(cmd)
			if err != nil {
				return fail(line, err)
			}
			stage.Steps = append(stage.Steps, sh)

		case "WORKDIR", "RUN", "COPY", "ADD", "REBOOT", "BREAKPOINT":
			if stage.Connect == nil {
				return fail(line, needsConnect(cmd.Keyword))
			}
			var (
				step instructions.Instruction
				err  error
			)
			switch cmd.Keyword {
			case "WORKDIR":
				step, err = instructions.ParseWorkdir(cmd, expand)
			case "RUN":
				step, err = instructions.ParseRun(cmd)
			case "COPY":
				step, err = instructions.ParseCopy(cmd, expand)
			case "ADD":
				step, err = instructions.ParseAdd(cmd, expand)
			case "REBOOT":
				step, err = instructions.ParseReboot(cmd)
			case "BREAKPOINT":
				step, err = instructions.ParseBreakpoint(cmd, expand)
			}
			if err != nil {
				return fail(line, err)
			}
			stage.usesVM = true
			stage.Steps = append(stage.Steps, step)

		case "SERVE":
			if stage.Serve != nil {
				return fail(line, fmt.Errorf("SERVE already set at line %d", stage.Serve.Line()))
			}
			if stage.ownConnect || stage.usesVM || len(stage.Boot) > 0 {
				return fail(line, fmt.Errorf("SERVE must come before BOOT, CONNECT and the instructions that need the VM: the server starts before the VM"))
			}
			sv, err := instructions.ParseServe(cmd, expand)
			if err != nil {
				return fail(line, err)
			}
			if k := stage.known(); sv.Addr != "" && stage.IsPacker() && (k == nil || k.HTTPIP == "") {
				return fail(line, fmt.Errorf("SERVE --addr is not supported in a plugin://%s stage: the plugin serves the directory and chooses its address", stage.Root().From.Source))
			}
			stage.Serve = sv

		case "BOOT":
			if stage.ownConnect || stage.usesVM {
				return fail(line, fmt.Errorf("BOOT must come before CONNECT and the instructions that need the VM: it types into the console before the connection"))
			}
			b, err := instructions.ParseBoot(cmd, expandBoot)
			if err != nil {
				return fail(line, err)
			}
			stage.Boot = append(stage.Boot, b)

		case "LABEL":
			if k := stage.known(); stage.IsPacker() && (k == nil || k.Tags == "") {
				return fail(line, fmt.Errorf("LABEL is not supported in a plugin://%s stage: the plugin configuration (FROM flags) sets its artifact (opendango maps LABEL for plugin://%s)", stage.Root().From.Source, packer.KnownSources()))
			}
			l, err := instructions.ParseLabel(cmd, expand)
			if err != nil {
				return fail(line, err)
			}
			if stage.labelLine == 0 {
				stage.labelLine = line
			}
			for _, kv := range l.Labels {
				stage.Labels = setLabel(stage.Labels, kv)
			}

		case "OUTPUT":
			k := stage.known()
			if stage.IsPacker() && k == nil {
				return fail(line, fmt.Errorf("OUTPUT has no meaning in a plugin://%s stage: the plugin configuration (FROM flags) sets its artifact (opendango maps OUTPUT for plugin://%s)", stage.Root().From.Source, packer.KnownSources()))
			}
			out, err := instructions.ParseOutput(cmd, expand)
			if err != nil {
				return fail(line, err)
			}
			if k != nil && out.Kind == instructions.OutputXVA {
				if k.XVAOutput == "" {
					return fail(line, fmt.Errorf("OUTPUT --format=xva is not supported by plugin://%s", stage.Root().From.Source))
				}
				for _, prev := range stage.Outputs {
					if prev.Kind == instructions.OutputXVA {
						return fail(line, fmt.Errorf("plugin://%s exports one XVA at most (already at line %d)", stage.Root().From.Source, prev.Line()))
					}
				}
			}
			for _, prev := range stage.Outputs {
				if out.Kind == instructions.OutputTemplate && prev.Kind == instructions.OutputTemplate {
					return fail(line, fmt.Errorf("only one OUTPUT --as=template is allowed (already set at line %d)", prev.Line()))
				}
				if out.Kind == instructions.OutputXVA && prev.Path == out.Path {
					return fail(line, fmt.Errorf("OUTPUT %s already written at line %d", out.Path, prev.Line()))
				}
			}
			stage.Outputs = append(stage.Outputs, out)

		default:
			return fail(line, fmt.Errorf("unknown instruction %s", cmd.Keyword))
		}
	}

	if stage == nil {
		return nil, &Error{File: file, Line: 1, Err: fmt.Errorf("no FROM instruction")}
	}
	for _, s := range p.Stages {
		if s.labelLine != 0 && s.TemplateOutput() == nil {
			return fail(s.labelLine, fmt.Errorf("LABEL needs OUTPUT --as=template: the labels become tags of the template"))
		}
	}
	for _, s := range p.Stages {
		switch {
		case len(s.Outputs) > 0 || len(s.Children) > 0:
		case !s.IsPacker():
			p.Warnings = append(p.Warnings, fmt.Sprintf("stage %s has no OUTPUT and no stage starts from it: its VM will be destroyed at the end of the build", s.Name()))
		case s.known() != nil:
			p.Warnings = append(p.Warnings, fmt.Sprintf("stage %s has no OUTPUT and no stage starts from it: the plugin's artifact will be deleted at the end of the stage", s.Name()))
		}
	}
	var unused []string
	for name := range buildArgs {
		if !usedArgs[name] {
			unused = append(unused, name)
		}
	}
	sort.Strings(unused)
	for _, name := range unused {
		p.Warnings = append(p.Warnings, fmt.Sprintf("--build-arg %s is not declared by any ARG", name))
	}
	return p, nil
}

// setLabel sets a label, in place when the key is already set.
func setLabel(labels []instructions.KeyValue, kv instructions.KeyValue) []instructions.KeyValue {
	for i := range labels {
		if labels[i].Key == kv.Key {
			labels[i].Value = kv.Value
			return labels
		}
	}
	return append(labels, kv)
}

// resolveArg sets a.Value: --build-arg first, then the default, then (in a
// stage) the global ARG of the same name.
func resolveArg(a *instructions.Arg, buildArgs, global map[string]string, isGlobal bool) {
	if v, ok := buildArgs[a.Name]; ok {
		a.Value, a.Set = v, true
		return
	}
	if a.HasDefault {
		a.Value, a.Set = a.Default, true
		return
	}
	if !isGlobal {
		a.Value, a.Set = global[a.Name]
	}
}

// checkBuilder validates the FROM flags with the builder of the stage's
// root, without contacting the infrastructure.
func checkBuilder(s *Stage) error {
	root := s.Root().From
	switch root.Builder {
	case "xo":
		var err error
		if s.Parent == nil {
			_, err = xo.ParseConfig(s.From.Source, s.From.Flags)
		} else {
			_, err = xo.ParseStageConfig(s.From.Flags)
		}
		return err
	case "plugin":
		flags, err := s.PackerFlags()
		if err != nil {
			return err
		}
		_, err = packer.ParseConfig(root.Source, flags)
		return err
	default:
		return fmt.Errorf("unknown builder %q (supported: xo, plugin)", root.Builder)
	}
}

// literalFlags returns cmd with the quotes left in its flag values escaped.
// BuildKit has already removed the quotes of the flags of standard
// instructions (FROM): the ones left, as in --network_names='["LAN"]', are
// part of the value, and must survive the ${VAR} expansion.
func literalFlags(cmd *parser.Command, escape rune) *parser.Command {
	esc := string(escape)
	r := strings.NewReplacer(esc, esc+esc, `"`, esc+`"`, "'", esc+"'")
	c := *cmd
	c.Flags = make([]string, len(cmd.Flags))
	for i, f := range cmd.Flags {
		name, value, ok := strings.Cut(f, "=")
		if ok {
			f = name + "=" + r.Replace(value)
		}
		c.Flags[i] = f
	}
	return &c
}

func unsetServeAddr() error {
	return fmt.Errorf("${%s} is only set after SERVE, in RUN (the remote shell expands it) and BOOT", instructions.ServeAddrVar)
}

func reserved(name string) error {
	return fmt.Errorf("%s is reserved: SERVE sets it", name)
}

func needsConnect(what string) error {
	return fmt.Errorf("%s needs a connection to the VM: add `CONNECT ssh --user=<user>` before it", what)
}

type envGetter map[string]string

func (e envGetter) Get(key string) (string, bool) {
	v, ok := e[key]
	return v, ok
}

func (e envGetter) Keys() []string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
