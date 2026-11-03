package build

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/littlejo/opendango/builders/packer"
	"github.com/littlejo/opendango/internal/instructions"
	"github.com/littlejo/opendango/internal/plan"
)

// FindPluginFunc returns the plugin and builder of a Packer source type.
type FindPluginFunc func(ctx context.Context, source string) (*packer.Plugin, string, error)

// FindPlugin looks for the plugin in packer.SearchDirs and PATH, and checks
// that it has the builder.
func FindPlugin(ctx context.Context, source string) (*packer.Plugin, string, error) {
	plugins := packer.Discover(packer.SearchDirs(), filepath.SplitList(os.Getenv("PATH")))
	pl, builder, err := packer.Find(plugins, source)
	if err != nil {
		return nil, "", err
	}
	d, err := pl.Describe(ctx)
	if err != nil {
		return nil, "", err
	}
	for _, b := range d.Builders {
		if b == builder {
			return pl, builder, nil
		}
	}
	return nil, "", fmt.Errorf("packer: %s has no builder %q (builders: %s)", pl.Path, builder, strings.Join(d.Builders, ", "))
}

// runPackerStage builds a FROM packer://<source> stage: the plugin creates
// the machine and connects to it, then calls back to run the instructions
// through its communicator, then produces its artifact.
func runPackerStage(ctx context.Context, p *plan.Plan, stage *plan.Stage, con *console, opts Options) (*executor, error) {
	cfg, err := packer.ParseConfig(stage.From.Source, stage.From.Flags)
	if err != nil {
		return nil, err
	}
	e := &executor{
		file:       p.File,
		name:       stage.Name(),
		out:        opts.Out,
		connect:    stage.Connect,
		contextDir: opts.ContextDir,
		console:    con,
	}

	e.say("Looking for the plugin of %s", cfg.Component)
	pl, builder, err := opts.FindPlugin(ctx, cfg.Component)
	if err != nil {
		return nil, err
	}
	e.say("Using %s", pl.Path)

	// CONNECT becomes the plugin's communicator; the credentials are FROM
	// flags.
	settings := cfg.Settings
	if c := e.connect; c != nil {
		e.user = c.User
		settings["communicator"] = "ssh"
		settings["ssh_username"] = c.User
		settings["ssh_port"] = c.Port
		settings["ssh_timeout"] = c.Timeout.String()
	} else {
		settings["communicator"] = "none"
	}
	// SERVE: the plugin serves the directory itself; boot_command reaches
	// it at {{ .HTTPIP }}:{{ .HTTPPort }}.
	if sv := stage.Serve; sv != nil {
		dir, err := serveDir(opts.ContextDir, sv)
		if err != nil {
			return nil, &plan.Error{File: p.File, Line: sv.Line(), Err: err}
		}
		settings["http_directory"] = dir
		if sv.Port != 0 {
			settings["http_port_min"] = sv.Port
			settings["http_port_max"] = sv.Port
		}
	}
	// BOOT: the plugin types boot_command, where ${DANGO_HTTP_ADDR} is the
	// address of its HTTP server.
	if len(stage.Boot) > 0 {
		var keys []string
		for _, boot := range stage.Boot {
			keys = append(keys, strings.ReplaceAll(boot.Keys, instructions.ServeAddrMarker, "{{ .HTTPIP }}:{{ .HTTPPort }}"))
		}
		settings["boot_command"] = keys
	}
	// What the plugin does when a step fails (its own cleanup).
	onError := "cleanup"
	switch {
	case opts.KeepOnError:
		onError = "abort"
	case opts.Ask:
		onError = "ask"
	}
	core := map[string]any{
		"packer_build_name":          stage.Name(),
		"packer_builder_type":        cfg.Component,
		"packer_core_version":        "opendango",
		"packer_debug":               false,
		"packer_force":               false,
		"packer_on_error":            onError,
		"packer_user_variables":      map[string]string{},
		"packer_sensitive_variables": cfg.Secrets,
	}

	ui := &packer.Ui{
		Print:    func(line string) { e.say("%s", line) },
		Question: func(q string) (string, error) { return e.pluginQuestion(ctx, q) },
		Secrets:  cfg.Secrets,
	}
	b := &packer.Build{
		Plugin:  pl,
		Builder: builder,
		Dir:     opts.ContextDir,
		Configs: []map[string]any{settings, core},
		Ui:      ui,
		Provision: func(ctx context.Context, comm *packer.Comm) error {
			e.say("Running the instructions")
			e.comm = comm
			defer func() { e.comm = nil }()
			return e.steps(ctx, p, stage, opts)
		},
	}
	artifact, err := b.Run(ctx)
	if err != nil {
		return nil, err
	}
	e.say("Artifact: %s", artifact)
	e.say("Stage finished")
	return e, nil
}

// pluginQuestion answers the plugin, which asks what to do after a failed
// step with packer_on_error=ask: "c" cleans up, "a" aborts without
// cleanup. A failed instruction was already answered (clean or keep);
// otherwise the user is asked.
func (e *executor) pluginQuestion(ctx context.Context, query string) (string, error) {
	c := e.answer
	if !e.answered {
		if e.console == nil {
			return "c", nil
		}
		e.console.mu.Lock()
		e.say("%s", strings.TrimSpace(query))
		c = ask(ctx, e.console.in, e.out, e.name, false)
		e.console.mu.Unlock()
	}
	if c == choiceKeep {
		return "a", nil
	}
	return "c", nil
}
