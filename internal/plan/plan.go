// Package plan turns a parsed Dangofile into a validated build plan: it
// resolves ARG values, expands variables and checks the stage-level rules.
package plan

import (
	"errors"
	"fmt"
	"maps"
	"slices"
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
	// FileFlags are the settings of dango.yaml for a plugin:// stage
	// (expanded); its FROM flags take precedence (PackerFlags).
	FileFlags map[string]string
	// ParentFlags are the dango.parent settings of a stage that starts from
	// another one, over FileFlags, with markers for the parent's artifact
	// (ExpandParent gives the values).
	ParentFlags map[string]string
	parent      *parentSettings
	// Index is the position of the stage in the file, from 0.
	Index int
	// Parent is the stage this one starts from (FROM <stage>); nil for
	// FROM <builder>://<source>.
	Parent *Stage
	// Children are the stages that start from this one.
	Children []*Stage
	// CopyFrom are the stages that COPY --from reads (they must be built
	// before), in the order of the Dangofile.
	CopyFrom []*Stage
	// Fetch are the paths that later stages COPY --from this one, sorted:
	// fetched at the end of its build.
	Fetch []string
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
	own := s.From.Flags
	if len(s.FileFlags) > 0 {
		// The FROM flags take precedence over dango.yaml.
		own = map[string]string{}
		for k, v := range s.FileFlags {
			own[k] = v
		}
		for k, v := range s.From.Flags {
			own[k] = v
		}
	}
	if s.Parent == nil {
		return own, nil
	}
	k := s.known()
	if s.ParentFlags == nil && (k == nil || k.Source == "") {
		return nil, fmt.Errorf("FROM %s: opendango does not know how plugin://%s starts from the artifact of a stage: set dango.parent in the %s section of %s (with ${%s} or ${%s})", s.From.Stage, s.Root().From.Source, s.Root().From.Source, SettingsFile, ParentIDVar, ParentFileVar)
	}
	flags := own
	if k != nil {
		// The infrastructure flags of the parent; the source ones are an
		// error.
		parent, err := s.Parent.PackerFlags()
		if err != nil {
			return nil, err
		}
		if flags, err = k.StageFlags(parent, own); err != nil {
			return nil, fmt.Errorf("FROM %s: %w", s.From.Stage, err)
		}
	}
	if s.ParentFlags != nil {
		// dango.parent over the section, under the FROM flags.
		merged := map[string]string{}
		for name, v := range flags {
			merged[name] = v
		}
		for name, v := range s.ParentFlags {
			if _, ok := s.From.Flags[name]; !ok {
				merged[name] = v
			}
		}
		flags = merged
	}
	return flags, nil
}

// parentSettings are the dango.parent settings of a stage as read, with
// what ExpandParent needs.
type parentSettings struct {
	file, source string
	raw          map[string]string
	global       map[string]string
	escape       rune
}

// ExpandParent returns the dango.parent settings of a stage that starts
// from another one, expanded with the global ARGs and the parent's
// artifact: its id and its first file (file is empty when it has none). A
// setting the stage sets with a FROM flag is left out. Shell patterns work
// (${DANGO_PARENT_ID#*:}).
func (s *Stage) ExpandParent(id, file string) (map[string]string, error) {
	ps := s.parent
	if ps == nil {
		return nil, nil
	}
	env := envGetter{ParentIDVar: id}
	if file != "" {
		env[ParentFileVar] = file
	}
	for k, v := range ps.global {
		env[k] = v
	}
	lex := shell.NewLex(ps.escape)
	esc := string(ps.escape)
	literal := strings.NewReplacer(esc, esc+esc, `"`, esc+`"`, "'", esc+"'")
	out := map[string]string{}
	for name, v := range ps.raw {
		if _, ok := s.From.Flags[name]; ok {
			continue
		}
		r, err := lex.ProcessWordWithMatches(literal.Replace(v), env)
		if err == nil {
			if _, ok := r.Unmatched[ParentFileVar]; ok {
				err = fmt.Errorf("${%s}: the artifact of stage %s has no file", ParentFileVar, s.Parent.Name())
			}
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %s.%s.parent.%s: %w", ps.file, ps.source, DangoKey, name, err)
		}
		out[name] = r.Result
	}
	return out, nil
}

// fileConnect reads the dango.connect option of a dango.yaml section, with
// the syntax of the CONNECT instruction.
func fileConnect(settings *Settings, source, value string, expand func(string) (string, error)) (*instructions.Connect, error) {
	res, err := parser.Parse(strings.NewReader("CONNECT " + value + "\n"))
	if err == nil && len(res.Commands) != 1 {
		err = errors.New("expected one line")
	}
	var c *instructions.Connect
	if err == nil {
		c, err = instructions.ParseConnect(res.Commands[0], expand)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %s.%s.connect: %w", settings.File, source, DangoKey, err)
	}
	return c, nil
}

// sectionFlags returns the dango.yaml settings of a plugin:// stage, with
// the variables expanded: quotes and escape characters are part of the
// value, as in the FROM flags (literalFlags). A stage that starts from
// another one leaves out the settings that only a root stage sets (its
// source is the parent).
func sectionFlags(settings *Settings, source string, section map[string]string, stage *Stage, escape rune, expand func(string) (string, error)) (map[string]string, error) {
	esc := string(escape)
	literal := strings.NewReplacer(esc, esc+esc, `"`, esc+`"`, "'", esc+"'")
	var skip []string
	if k := stage.known(); stage != nil && k != nil && stage.Parent != nil {
		skip = append([]string{k.Source}, k.RootOnly...)
	}
	flags := map[string]string{}
	for name, v := range section {
		if slices.Contains(skip, name) {
			continue
		}
		value, err := expand(literal.Replace(v))
		if err != nil {
			return nil, fmt.Errorf("%s: %s.%s: %w", settings.File, source, name, err)
		}
		flags[name] = value
	}
	return flags, nil
}

// known returns what opendango knows of the Packer builder of the stage,
// nil for a native stage or a builder that is not in the table.
func (s *Stage) known() *packer.Known {
	if s == nil || !s.IsPacker() {
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
		// The stage, its ancestors and the stages they COPY --from (and
		// theirs), in file order.
		need := map[*Stage]bool{}
		var add func(*Stage)
		add = func(a *Stage) {
			for ; a != nil && !need[a]; a = a.Parent {
				need[a] = true
				for _, src := range a.CopyFrom {
					add(src)
				}
			}
		}
		add(s)
		var chain []*Stage
		for _, st := range p.Stages {
			if need[st] {
				chain = append(chain, st)
			}
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
	return BuildSettings(file, res, buildArgs, nil)
}

// BuildSettings is Build with the plugin settings of a dango.yaml file
// (nil for none).
func BuildSettings(file string, res *parser.Result, buildArgs map[string]string, settings *Settings) (*Plan, error) {
	p := &Plan{File: file}
	usedSections := map[string]bool{}
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
				if a.Name == ParentIDVar || a.Name == ParentFileVar {
					return fail(line, fmt.Errorf("%s is reserved: dango.parent in %s sets it", a.Name, SettingsFile))
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
			// The dango.yaml section of the source, with the global ARGs.
			if settings != nil && stage.IsPacker() {
				source := stage.Root().From.Source
				if section, ok := settings.Sections[source]; ok {
					usedSections[source] = true
					if stage.FileFlags, err = sectionFlags(settings, source, section, stage, res.EscapeToken, expand); err != nil {
						return nil, err
					}
				}
				// How a child starts from its parent's artifact: markers
				// for ${DANGO_PARENT_ID} and ${DANGO_PARENT_FILE}, replaced
				// at build time.
				if parent := settings.Dango[source].Parent; parent != nil && stage.Parent != nil {
					env := envGetter{ParentIDVar: ParentIDMarker, ParentFileVar: ParentFileMarker}
					for k, v := range global {
						env[k] = v
					}
					expandParent := func(word string) (string, error) {
						r, err := lex.ProcessWordWithMatches(word, env)
						return r.Result, err
					}
					if stage.ParentFlags, err = sectionFlags(settings, source, parent, nil, res.EscapeToken, expandParent); err != nil {
						return nil, err
					}
					// Expanded again at build time, with the artifact.
					stage.parent = &parentSettings{file: settings.File, source: source, raw: parent, global: maps.Clone(global), escape: res.EscapeToken}
				}
				// Its default CONNECT, unless the parent gives one; a
				// CONNECT in the stage replaces it.
				if c := settings.Dango[source].Connect; c != "" && stage.Connect == nil {
					if stage.Connect, err = fileConnect(settings, source, c, expand); err != nil {
						return nil, err
					}
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
				return fail(line, needsConnect("ENV --persist", stage))
			}
			stage.usesVM = stage.usesVM || env.Persist
			for _, kv := range env.Vars {
				if kv.Key == instructions.ServeAddrVar {
					return fail(line, reserved(kv.Key))
				}
				if kv.Key == ParentIDVar || kv.Key == ParentFileVar {
					return fail(line, fmt.Errorf("%s is reserved: dango.parent in %s sets it", kv.Key, SettingsFile))
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
				return fail(line, needsConnect(cmd.Keyword, stage))
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
				var c *instructions.Copy
				if c, err = instructions.ParseCopy(cmd, expand); err == nil && c.From != "" {
					err = copyFrom(stage, c, byName)
				}
				step = c
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
			out, err := instructions.ParseOutput(cmd, expand)
			if err != nil {
				return fail(line, err)
			}
			// The artifact itself (template, XVA) needs a builder opendango
			// knows; the post-processors work with any.
			if stage.IsPacker() && k == nil && !out.PostProcess() {
				return fail(line, fmt.Errorf("OUTPUT --%s has no meaning in a plugin://%s stage: the plugin configuration (FROM flags) sets its artifact (opendango maps it for plugin://%s)", outputFlag(out), stage.Root().From.Source, packer.KnownSources()))
			}
			if k != nil && out.Kind == instructions.OutputXVA && k.XVA == nil {
				return fail(line, fmt.Errorf("OUTPUT --format=xva is not supported by plugin://%s", stage.Root().From.Source))
			}
			for _, prev := range stage.Outputs {
				if out.Kind == instructions.OutputTemplate && prev.Kind == instructions.OutputTemplate {
					return fail(line, fmt.Errorf("only one OUTPUT --as=template is allowed (already set at line %d)", prev.Line()))
				}
				if out.Path != "" && prev.Path == out.Path && out.Kind != instructions.OutputPlugin {
					return fail(line, fmt.Errorf("OUTPUT %s already written at line %d", out.Path, prev.Line()))
				}
			}
			// A plugin's post-processor: its dango.yaml section under its
			// flags.
			if out.Kind == instructions.OutputPlugin && settings != nil {
				if section, ok := settings.Sections[out.Source]; ok {
					usedSections[out.Source] = true
					flags, err := sectionFlags(settings, out.Source, section, nil, res.EscapeToken, expand)
					if err != nil {
						return nil, err
					}
					for name, v := range out.Settings {
						flags[name] = v
					}
					out.Settings = flags
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
		case len(s.Outputs) > 0 || len(s.Children) > 0 || len(s.Fetch) > 0:
		case !s.IsPacker():
			p.Warnings = append(p.Warnings, fmt.Sprintf("stage %s has no OUTPUT and no stage starts from it: its VM will be destroyed at the end of the build", s.Name()))
		case s.known() != nil:
			p.Warnings = append(p.Warnings, fmt.Sprintf("stage %s has no OUTPUT and no stage starts from it: the plugin's artifact will be deleted at the end of the stage", s.Name()))
		}
	}
	if settings != nil {
		var sources []string
		for source := range settings.Sections {
			if !usedSections[source] {
				sources = append(sources, source)
			}
		}
		sort.Strings(sources)
		for _, source := range sources {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s: no stage uses plugin://%s", settings.File, source))
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

// copyFrom records a COPY --from: the stage it reads must come earlier,
// and fetches the paths at the end of its build.
func copyFrom(stage *Stage, c *instructions.Copy, byName map[string]*Stage) error {
	src, ok := byName[c.From]
	if !ok {
		return fmt.Errorf("COPY --from=%s: no earlier stage named %q (images are not supported)", c.From, c.From)
	}
	if src == stage {
		return fmt.Errorf("COPY --from=%s: a stage cannot copy from itself", c.From)
	}
	if src.Connect == nil {
		return fmt.Errorf("COPY --from=%s: stage %s has no connection to fetch the files through", c.From, c.From)
	}
	if !slices.Contains(stage.CopyFrom, src) {
		stage.CopyFrom = append(stage.CopyFrom, src)
	}
	for _, p := range c.Sources {
		if !slices.Contains(src.Fetch, p) {
			src.Fetch = append(src.Fetch, p)
		}
	}
	slices.Sort(src.Fetch)
	return nil
}

// outputFlag names an output for the messages: as=template or format=xva.
func outputFlag(o *instructions.Output) string {
	if o.Kind == instructions.OutputTemplate {
		return "as=template"
	}
	return "format=" + o.Kind
}

func needsConnect(what string, stage *Stage) error {
	if stage.IsPacker() {
		return fmt.Errorf("%s needs a connection to the VM: add `CONNECT ssh --user=<user>` before it, or set dango.connect in the %s section of %s", what, stage.Root().From.Source, SettingsFile)
	}
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
