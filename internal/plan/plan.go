// Package plan turns a parsed Dangofile into a validated build plan: it
// resolves ARG values, expands variables and checks the stage-level rules.
package plan

import (
	"fmt"
	"sort"

	"github.com/moby/buildkit/frontend/dockerfile/shell"

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
	// Steps run in order: *Arg, *Env, *User, *Workdir, *Run, *Copy, *Reboot.
	Steps []instructions.Instruction
	// Outputs run after all steps, once the VM is stopped.
	Outputs []*instructions.Output

	// ownConnect is true when the stage sets CONNECT itself.
	ownConnect bool
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
	expand := func(word string) (string, error) {
		res, err := lex.ProcessWordWithMatches(word, envGetter(scope))
		return res.Result, err
	}

	var stage *Stage
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
			from, err := instructions.ParseFrom(cmd, expand)
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
			for _, kv := range env.Vars {
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

		case "WORKDIR", "RUN", "COPY", "REBOOT":
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
			case "REBOOT":
				step, err = instructions.ParseReboot(cmd)
			}
			if err != nil {
				return fail(line, err)
			}
			stage.Steps = append(stage.Steps, step)

		case "OUTPUT":
			out, err := instructions.ParseOutput(cmd, expand)
			if err != nil {
				return fail(line, err)
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
		if len(s.Outputs) == 0 && len(s.Children) == 0 {
			p.Warnings = append(p.Warnings, fmt.Sprintf("stage %s has no OUTPUT and no stage starts from it: its VM will be destroyed at the end of the build", s.Name()))
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
	default:
		return fmt.Errorf("unknown builder %q (supported: xo)", root.Builder)
	}
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
