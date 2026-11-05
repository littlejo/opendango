package build

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	return nil, "", fmt.Errorf("plugin: %s has no builder %q (builders: %s)", pl.Path, builder, strings.Join(d.Builders, ", "))
}

// runPackerStage builds a FROM plugin://<source> stage, or a stage that
// starts from one: the plugin creates the machine and connects to it, then
// calls back to run the instructions through its communicator, then
// produces its artifact. parent and needed are those of runStage.
func runPackerStage(ctx context.Context, p *plan.Plan, stage *plan.Stage, parent *executor, needed bool, con *console, opts Options) (*executor, error) {
	flags, err := stage.PackerFlags()
	if err != nil {
		return nil, err
	}
	cfg, err := packer.ParseConfig(stage.Root().From.Source, flags)
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
	if parent != nil {
		// Like a native stage: USER, SHELL, WORKDIR and ENV carry over.
		e.user, e.shell, e.workdir = parent.user, parent.shell, parent.workdir
		e.env = append([]instructions.KeyValue{}, parent.env...)
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
		if e.user == "" {
			e.user = c.User
		}
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
	// OUTPUT and LABEL become the settings of the builder's artifact; without
	// OUTPUT --as=template, the artifact is deleted once built, like the VM
	// of a native stage, or at the end of the build when other stages start
	// from it (an intermediate template, like with --no-cache: plugin://
	// stages are not cached).
	destroy, intermediate := false, false
	if k := packer.Lookup(cfg.Component); k != nil {
		if err := knownSettings(settings, k, stage, opts.ContextDir); err != nil {
			return nil, err
		}
		if parent != nil {
			settings[k.Source] = parent.artifact.ID()
		}
		if sv := stage.Serve; sv != nil && sv.Addr != "" {
			settings[k.HTTPIP] = sv.Addr
		}
		if len(stage.Boot) > 0 && k.BootWait != "" {
			settings[k.BootWait] = "-1s"
		}
		if stage.TemplateOutput() == nil {
			if needed {
				intermediate = true
				settings[k.TemplateName] = fmt.Sprintf("opendango-tmp-%s-%d", stage.Name(), time.Now().Unix())
			} else {
				destroy = true
			}
		}
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
		Plugin:          pl,
		Builder:         builder,
		Dir:             opts.ContextDir,
		Configs:         []map[string]any{settings, core},
		Ui:              ui,
		DestroyArtifact: destroy,
		KeepPlugin:      intermediate,
		Provision: func(ctx context.Context, comm *packer.Comm, data packer.HookData) error {
			e.say("Running the instructions")
			e.comm = comm
			defer func() { e.comm = nil }()
			// BREAKPOINT and --on-error=ask print the ssh command of the
			// plugin's connection while it runs the instructions; the
			// plugin removes its key afterwards.
			if data.Host != "" {
				e.host, e.plugin = data.Host, &data
				defer func() { e.host, e.plugin = "", nil }()
			}
			// DANGO_HTTP_ADDR is the address of the plugin's server, when
			// the plugin tells it.
			if stage.Serve != nil {
				if data.HTTPAddr == "" {
					e.say("Warning: the plugin does not tell the address of its HTTP server: DANGO_HTTP_ADDR is not set in RUN")
				}
				e.serveAddr = data.HTTPAddr
			}
			return e.steps(ctx, p, stage, opts)
		},
	}
	artifact, err := b.Run(ctx)
	if err != nil {
		return nil, err
	}
	if artifact == nil {
		if needed {
			return nil, fmt.Errorf("plugin: %s produced no artifact for the stages that start from %s", pl.Name, stage.Name())
		}
		e.say("No artifact")
		e.say("Stage finished")
		return e, nil
	}
	e.artifact = &packerTemplate{artifact}
	e.say("Artifact: %s", artifact.Desc)
	e.say("Stage finished")
	return e, nil
}

// packerTemplate is the artifact of a plugin:// stage that other stages
// start from.
type packerTemplate struct{ a *packer.Artifact }

func (t *packerTemplate) ID() string                        { return t.a.ID }
func (t *packerTemplate) Destroy(ctx context.Context) error { return t.a.Destroy() }

// close stops the plugin kept for Destroy.
func (t *packerTemplate) close() { t.a.Close() }

// knownSettings sets the settings that OUTPUT and LABEL become for a builder
// of the table. The plan checked that the builder supports them.
func knownSettings(settings map[string]any, k *packer.Known, stage *plan.Stage, contextDir string) error {
	for _, out := range stage.Outputs {
		switch out.Kind {
		case instructions.OutputTemplate:
			if out.Name != "" {
				settings[k.TemplateName] = out.Name
			}
		case instructions.OutputXVA:
			path := out.Path
			if !filepath.IsAbs(path) {
				path = filepath.Join(contextDir, path)
			}
			settings[k.XVAOutput] = path
			if out.Compress != "" {
				settings[k.XVACompress] = out.Compress
			}
		default:
			return fmt.Errorf("plugin: unsupported output %q", out.Kind)
		}
	}
	if len(stage.Labels) > 0 {
		tags := make([]any, 0, len(stage.Labels))
		for _, l := range stage.Labels {
			tags = append(tags, l.Key+"="+l.Value)
		}
		settings[k.Tags] = tags
	}
	return nil
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
