package build

import (
	"context"
	"errors"
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
func runPackerStage(ctx context.Context, p *plan.Plan, stage *plan.Stage, parent *executor, needed bool, key string, from map[string]string, con *console, opts Options) (*executor, error) {
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
		fetchRoot:  filepath.Join(opts.filesDir, fmt.Sprint(stage.Index)),
		fetched:    from,
		files:      filesCache(opts.FilesCache),

		findProvisioner: opts.FindProvisioner,
		builderType:     cfg.Component,
	}
	if key != "" && len(stage.Fetch) > 0 {
		e.filesKey = key
	}
	if parent != nil {
		// Like a native stage: USER, SHELL, WORKDIR and ENV carry over.
		e.user, e.shell, e.workdir = parent.user, parent.shell, parent.workdir
		e.env = append([]instructions.KeyValue{}, parent.env...)
	}
	// A stage that only feeds COPY --from, with its files in the cache:
	// nothing to build, whatever the plugin.
	if e.filesKey != "" && filesOnly(stage, needed) && e.files.has(key, stage.Fetch) {
		e.say("Using the cached files of COPY --from (key %s)", key[:12])
		e.fetchRoot = e.files.root(key)
		e.replay(stage.Steps)
		e.say("Stage finished (cached)")
		return e, nil
	}

	e.say("Looking for the plugin of %s", cfg.Component)
	pl, builder, err := opts.FindPlugin(ctx, cfg.Component)
	if err != nil {
		return nil, err
	}
	e.say("Using %s", pl.Path)

	if e.connect != nil && e.user == "" {
		e.user = e.connect.User
	}
	parentID := ""
	if parent != nil {
		parentID = parent.artifact.ID()
	}
	// With dango.parent, the user tells how the plugin starts from the
	// parent's artifact; otherwise the table does (its Source setting).
	tableSource := parentID
	if stage.ParentFlags != nil {
		tableSource = ""
	}
	settings, err := stageSettings(p.File, stage, cfg, tableSource, opts.ContextDir)
	if err != nil {
		return nil, err
	}
	if parent != nil {
		if err := parentSettings(settings, parent.artifact, stage, cfg.Component); err != nil {
			return nil, err
		}
	}
	if packer.Lookup(cfg.Component) == nil {
		// Its own flags set the artifact (a file, an AMI…): it stays, also
		// when other stages start from it.
		e.keepArtifact = true
	}
	// OUTPUT and LABEL become the settings of the builder's artifact; without
	// OUTPUT --as=template, the artifact is deleted once built, like the VM
	// of a native stage, or ends as an intermediate template when other
	// stages start from it.
	destroy, intermediate := false, false
	if k := packer.Lookup(cfg.Component); k != nil {
		source := flags[k.Source]
		if parent != nil {
			source = parentID
		}

		// The cache: a template tagged with the key, found by the plugin's
		// data source.
		if key != "" && k.Cache != nil && !filesOnly(stage, needed) {
			found, err := packerCache(pl, k.Cache, flags, source, key, opts.ContextDir)
			if err != nil {
				return nil, fmt.Errorf("looking up the cache: %w", err)
			}
			if found != nil && e.filesKey != "" {
				if e.files.has(key, stage.Fetch) {
					e.fetchRoot = e.files.root(key)
				} else if err := packerFetchFromTemplate(ctx, p, stage, pl, builder, k, cfg, flags, found.ID, e, opts); err != nil {
					return nil, err
				}
			}
			if found != nil {
				e.say("Using the cached template %s (key %s)", found.ID, key[:12])
				e.cached = true
				e.artifact = &packerTemplate{found}
				// The child stages inherit the final USER, SHELL, WORKDIR and
				// ENV.
				e.replay(stage.Steps)
				// The XVA OUTPUTs, exported from the cached template.
				if err := packerXVA(ctx, p, stage, pl, k, flags, found.ID, cfg.Secrets, e, opts); err != nil {
					return nil, err
				}
				m := made{ID: found.ID, BuilderType: cfg.Component, Files: xvaPaths(stage, opts.ContextDir)}
				if err := postProcess(ctx, p, stage, m, e, opts); err != nil {
					return nil, err
				}
				e.say("Stage finished (cached)")
				return e, nil
			}
		} else if !filesOnly(stage, needed) {
			key = ""
		}

		// The labels, then the cache tags (the creation one first: a
		// template with the key but without it would never be pruned).
		tags, _ := settings[k.Tags].([]any)
		name := ""
		switch {
		case stage.TemplateOutput() != nil:
			if key != "" {
				tags = append(tags, CacheTagPrefix+key)
			}
		case needed && key != "":
			now := time.Now().Unix()
			name = fmt.Sprintf("opendango-cache-%s-%d", stage.Name(), now)
			tags = append(tags, fmt.Sprintf("%s%d", CreatedTagPrefix, now), CacheTagPrefix+key)
		case needed:
			// Not cached: deleted at the end of the build, through the
			// plugin that made it.
			intermediate = true
			name = fmt.Sprintf("opendango-tmp-%s-%d", stage.Name(), time.Now().Unix())
		default:
			destroy = true
		}
		if name != "" {
			settings[k.TemplateName] = name
		}
		if len(tags) > 0 {
			settings[k.Tags] = tags
		}
		if key != "" {
			// What was fixed by hand before a retry is not reproducible,
			// and the cache tags are set by the plugin at the end.
			e.noRetry = true
			e.cached = true
		}
	}
	core := coreSettings(stage, cfg, opts)

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
		// The XVA OUTPUTs are exported from the artifact before it is
		// deleted: through the plugin, kept until then.
		DestroyArtifact: destroy && !hasPostOutputs(stage),
		KeepPlugin:      intermediate || (destroy && hasPostOutputs(stage)),
		Provision: func(ctx context.Context, comm *packer.Comm, data packer.HookData) error {
			e.say("Running the instructions")
			e.comm = comm
			defer func() { e.comm = nil }()
			// PROVISION passes the plugin's data on the connection.
			e.pluginData = data.Raw
			defer func() { e.pluginData = nil }()
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
			if err := e.steps(ctx, p, stage, opts); err != nil {
				return err
			}
			// The files later stages COPY --from, while the machine runs.
			return e.fetchFiles(ctx, stage)
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
	if hasPostOutputs(stage) {
		var err error
		if hasXVA(stage) {
			err = packerXVA(ctx, p, stage, pl, packer.Lookup(cfg.Component), flags, artifact.ID, cfg.Secrets, e, opts)
		}
		if err == nil {
			// The plugin's files are relative to its directory, the
			// Dangofile's.
			m := made{ID: artifact.ID, BuilderID: artifact.BuilderID, BuilderType: cfg.Component}
			for _, f := range artifact.Files {
				if !filepath.IsAbs(f) {
					f = filepath.Join(opts.ContextDir, f)
				}
				m.Files = append(m.Files, f)
			}
			m.Files = append(m.Files, xvaPaths(stage, opts.ContextDir)...)
			err = postProcess(ctx, p, stage, m, e, opts)
		}
		if destroy {
			e.say("Deleting the artifact: no OUTPUT --as=template")
			if derr := artifact.Destroy(); derr != nil {
				err = errors.Join(err, derr)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	e.say("Stage finished")
	return e, nil
}

// stageSettings returns the plugin settings of a stage: its FROM flags,
// plus what CONNECT, SERVE and BOOT become, and for a builder of the table
// OUTPUT, LABEL, SERVE --addr and the source (parentID, the parent's
// artifact, for a stage that starts from another one). The cache tags and
// the intermediate template name come on top, in runPackerStage.
func stageSettings(file string, stage *plan.Stage, cfg *packer.Config, parentID, contextDir string) (map[string]any, error) {
	settings := map[string]any{}
	for k, v := range cfg.Settings {
		settings[k] = v
	}
	// CONNECT becomes the plugin's communicator; the credentials are FROM
	// flags.
	if c := stage.Connect; c != nil {
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
		dir, err := serveDir(contextDir, sv)
		if err != nil {
			return nil, &plan.Error{File: file, Line: sv.Line(), Err: err}
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
	k := packer.Lookup(cfg.Component)
	if k == nil {
		return settings, nil
	}
	if err := knownSettings(settings, k, stage, contextDir); err != nil {
		return nil, err
	}
	if parentID != "" {
		settings[k.Source] = parentID
	}
	if sv := stage.Serve; sv != nil && sv.Addr != "" {
		settings[k.HTTPIP] = sv.Addr
	}
	if len(stage.Boot) > 0 && k.BootWait != "" {
		settings[k.BootWait] = "-1s"
	}
	// The cache tags of the source are not the machine's.
	if k.DropInheritedTags != "" {
		settings[k.DropInheritedTags] = []any{CacheTagPrefix, CreatedTagPrefix}
	}
	return settings, nil
}

// coreSettings returns the packer_* settings Packer's core passes to a
// builder.
func coreSettings(stage *plan.Stage, cfg *packer.Config, opts Options) map[string]any {
	// What the plugin does when a step fails (its own cleanup).
	onError := "cleanup"
	switch {
	case opts.KeepOnError:
		onError = "abort"
	case opts.Ask:
		onError = "ask"
	}
	return map[string]any{
		"packer_build_name":          stage.Name(),
		"packer_builder_type":        cfg.Component,
		"packer_core_version":        "opendango",
		"packer_debug":               false,
		"packer_force":               false,
		"packer_on_error":            onError,
		"packer_user_variables":      map[string]string{},
		"packer_sensitive_variables": cfg.Secrets,
	}
}

// packerTemplate is the artifact of a plugin:// stage that other stages
// start from.
type packerTemplate struct{ a *packer.Artifact }

func (t *packerTemplate) ID() string                        { return t.a.ID }
func (t *packerTemplate) Destroy(ctx context.Context) error { return t.a.Destroy() }

// close stops the plugin kept for Destroy.
func (t *packerTemplate) close() { t.a.Close() }

// packerXVA runs the XVA OUTPUTs of a stage of a known builder: the
// plugin's post-processor exports the template id, once per OUTPUT.
func packerXVA(ctx context.Context, p *plan.Plan, stage *plan.Stage, pl *packer.Plugin, k *packer.Known, flags map[string]string, id string, secrets []string, e *executor, opts Options) error {
	for _, out := range stage.Outputs {
		if out.Kind != instructions.OutputXVA {
			continue
		}
		path := out.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(opts.ContextDir, path)
		}
		e.say("Exporting XVA to %s", path)
		cfg := map[string]any{k.XVA.Output: path}
		if out.Compress != "" {
			cfg[k.XVA.Compress] = out.Compress
		}
		for _, name := range k.XVA.Copy {
			if v, ok := flags[name]; ok {
				cfg[name] = v
			}
		}
		ui := &packer.Ui{Print: func(line string) { e.say("%s", line) }, Secrets: secrets}
		o, err := packer.PostProcess(ctx, pl, k.XVA.PostProcessor, opts.ContextDir, ui, packer.Input{ID: id}, cfg)
		if err != nil {
			return &plan.Error{File: p.File, Line: out.Line(), Err: err}
		}
		o.Close()
	}
	return nil
}

// parentSettings sets the dango.parent settings of a stage that starts from
// another one, expanded with the parent's artifact (its id and first
// file), over the section and under the FROM flags.
func parentSettings(settings map[string]any, parent *packerTemplate, stage *plan.Stage, source string) error {
	file := ""
	if files := parent.a.Files; len(files) > 0 {
		file = files[0]
	}
	values, err := stage.ExpandParent(parent.ID(), file)
	if err != nil || len(values) == 0 {
		return err
	}
	// The JSON values become lists and maps, like FROM flags.
	cfg, err := packer.ParseConfig(source, values)
	if err != nil {
		return err
	}
	for k, v := range cfg.Settings {
		settings[k] = v
	}
	return nil
}

// packerFetchFromTemplate fetches the files of a stage of a known builder
// found in the cache as a template, without its files: the plugin builds a
// machine from the template (a child stage without instructions), the files
// are fetched, its artifact is deleted.
func packerFetchFromTemplate(ctx context.Context, p *plan.Plan, stage *plan.Stage, pl *packer.Plugin, builder string, k *packer.Known, cfg *packer.Config, flags map[string]string, id string, e *executor, opts Options) error {
	e.say("The files of COPY --from are not in the cache: fetching them from the cached template %s", id)
	synth := fetchStage(stage)
	inherited := map[string]string{}
	for _, name := range k.Inherited {
		if v, ok := flags[name]; ok {
			inherited[name] = v
		}
	}
	scfg, err := packer.ParseConfig(cfg.Component, inherited)
	if err != nil {
		return err
	}
	settings, err := stageSettings(p.File, synth, scfg, id, opts.ContextDir)
	if err != nil {
		return err
	}
	settings[k.TemplateName] = fmt.Sprintf("opendango-fetch-%s-%d", stage.Name(), time.Now().Unix())
	fe := &executor{file: e.file, name: e.name, out: e.out, connect: stage.Connect, contextDir: e.contextDir, console: e.console,
		filesKey: e.filesKey, files: e.files, fetchRoot: e.fetchRoot}
	ui := &packer.Ui{Print: func(line string) { fe.say("%s", line) }, Secrets: cfg.Secrets}
	b := &packer.Build{
		Plugin:          pl,
		Builder:         builder,
		Dir:             opts.ContextDir,
		Configs:         []map[string]any{settings, coreSettings(synth, scfg, opts)},
		Ui:              ui,
		DestroyArtifact: true,
		Provision: func(ctx context.Context, comm *packer.Comm, _ packer.HookData) error {
			fe.comm = comm
			defer func() { fe.comm = nil }()
			return fe.fetchFiles(ctx, stage)
		},
	}
	if _, err := b.Run(ctx); err != nil {
		return err
	}
	e.fetchRoot = fe.fetchRoot
	return nil
}

// hasPostOutputs tells whether the stage has outputs that run on the
// artifact once made: XVA exports and post-processors.
func hasPostOutputs(stage *plan.Stage) bool {
	for _, out := range stage.Outputs {
		if out.Kind == instructions.OutputXVA || out.PostProcess() {
			return true
		}
	}
	return false
}

// xvaPaths returns the absolute paths of the XVA OUTPUTs of a stage.
func xvaPaths(stage *plan.Stage, contextDir string) []string {
	var paths []string
	for _, out := range stage.Outputs {
		if out.Kind != instructions.OutputXVA {
			continue
		}
		path := out.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(contextDir, path)
		}
		paths = append(paths, path)
	}
	return paths
}

// hasXVA tells whether the stage has an OUTPUT --format=xva.
func hasXVA(stage *plan.Stage) bool {
	for _, out := range stage.Outputs {
		if out.Kind == instructions.OutputXVA {
			return true
		}
	}
	return false
}

// packerCache looks for the template tagged with the cache key, where a
// machine created from source would be, with the plugin's data source. It
// returns nil when there is none.
func packerCache(pl *packer.Plugin, c *packer.CacheLookup, flags map[string]string, source, key, dir string) (*packer.Artifact, error) {
	cfg := map[string]any{c.Tags: []any{CacheTagPrefix + key}, c.AllowMissing: true}
	for _, name := range c.Copy {
		if v, ok := flags[name]; ok {
			cfg[name] = v
		}
	}
	if source != "" {
		cfg[c.SamePoolAs] = source
	}
	out, err := packer.Datasource(pl, c.Datasource, dir, cfg)
	if err != nil {
		return nil, err
	}
	if out[c.ID] == "" {
		return nil, nil
	}
	return &packer.Artifact{ID: out[c.ID], Desc: out[c.Name]}, nil
}

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
			// The plugin's post-processor, once built (packerXVA).
		case instructions.OutputManifest, instructions.OutputChecksum, instructions.OutputArchive, instructions.OutputPlugin:
			// Once built (postProcess).
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
