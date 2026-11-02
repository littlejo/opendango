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

// Plan is a validated Dangofile. The MVP supports a single stage.
type Plan struct {
	File     string
	Stage    *Stage
	Warnings []string
}

// Stage is everything between a FROM and the end of the file.
type Stage struct {
	From *instructions.From
	// Connect is nil when no instruction needs a connection.
	Connect *instructions.Connect
	// Steps run in order: *Arg, *Env, *User, *Workdir, *Run, *Copy, *Reboot.
	Steps []instructions.Instruction
	// Outputs run after all steps, once the VM is stopped.
	Outputs []*instructions.Output
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
			if stage != nil {
				return fail(line, fmt.Errorf("multiple stages are not supported yet"))
			}
			from, err := instructions.ParseFrom(cmd, expand)
			if err != nil {
				return fail(line, err)
			}
			if err := checkBuilder(from); err != nil {
				return fail(line, err)
			}
			stage = &Stage{From: from}
			p.Stage = stage
			scope = map[string]string{}

		case "CONNECT":
			if stage.Connect != nil {
				return fail(line, fmt.Errorf("CONNECT already set at line %d", stage.Connect.Line()))
			}
			c, err := instructions.ParseConnect(cmd, expand)
			if err != nil {
				return fail(line, err)
			}
			stage.Connect = c

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
	if len(stage.Outputs) == 0 {
		p.Warnings = append(p.Warnings, "no OUTPUT: the VM will be destroyed at the end of the build")
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

func checkBuilder(from *instructions.From) error {
	switch from.Builder {
	case "xo":
		_, err := xo.ParseConfig(from.Source, from.Flags)
		return err
	default:
		return fmt.Errorf("unknown builder %q (supported: xo)", from.Builder)
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
