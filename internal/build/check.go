package build

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/hashicorp/packer-plugin-sdk/didyoumean"

	"github.com/littlejo/opendango/builders/packer"
	"github.com/littlejo/opendango/internal/instructions"
	"github.com/littlejo/opendango/internal/plan"
)

// CheckPlugins checks the FROM flags of the plugin:// stages with their
// plugins, without building: a flag that is not a setting of the builder
// (with the closest one), then the builder's Prepare, which checks the
// configuration without creating anything. A plugin that is not installed
// only gives a warning: the stage is not checked.
func CheckPlugins(ctx context.Context, p *plan.Plan, contextDir string, f Finders) (warnings []string, err error) {
	find := f.Builder
	if find == nil {
		find = FindPlugin
	}
	if f.PostProcessor == nil {
		f.PostProcessor = FindPostProcessor
	}
	if f.Provisioner == nil {
		f.Provisioner = FindProvisioner
	}
	specs := map[string]map[string]bool{}
	var errs []error
	for _, stage := range p.Stages {
		// The plugins' post-processors check their settings (Configure).
		for _, out := range stage.Outputs {
			if out.Kind != instructions.OutputPlugin {
				continue
			}
			cfg, err := packer.ParseConfig(out.Source, out.Settings)
			if err == nil {
				pl, name, ferr := f.PostProcessor(ctx, cfg.Component)
				if ferr != nil {
					warnings = append(warnings, fmt.Sprintf("stage %s: plugin://%s is not checked: %v", stage.Name(), cfg.Component, ferr))
					continue
				}
				core := map[string]any{"packer_build_name": stage.Name(), "packer_builder_type": stage.Root().From.Builder}
				err = packer.ConfigurePostProcessor(pl, name, contextDir, cfg.Settings, core)
			}
			if err != nil {
				errs = append(errs, &plan.Error{File: p.File, Line: out.Line(), Err: fmt.Errorf("plugin://%s: %w", out.Source, err)})
			}
		}
		// The provisioners check their settings (Prepare).
		for _, step := range stage.Steps {
			pr, ok := step.(*instructions.Provision)
			if !ok {
				continue
			}
			cfg, err := packer.ParseConfig(pr.Source, pr.Settings)
			if err == nil {
				pl, name, ferr := f.Provisioner(ctx, cfg.Component)
				if ferr != nil {
					warnings = append(warnings, fmt.Sprintf("stage %s: plugin://%s is not checked: %v", stage.Name(), cfg.Component, ferr))
					continue
				}
				core := map[string]any{"packer_build_name": stage.Name(), "packer_builder_type": stage.Root().From.Builder}
				err = packer.PrepareProvisioner(pl, name, contextDir, cfg.Settings, core)
			}
			if err != nil {
				errs = append(errs, &plan.Error{File: p.File, Line: pr.Line(), Err: fmt.Errorf("plugin://%s: %w", pr.Source, err)})
			}
		}
		if !stage.IsPacker() {
			continue
		}
		line := stage.From.Line()
		fail := func(err error) { errs = append(errs, &plan.Error{File: p.File, Line: line, Err: err}) }
		flags, err := stage.PackerFlags()
		if err != nil {
			fail(err)
			continue
		}
		cfg, err := packer.ParseConfig(stage.Root().From.Source, flags)
		if err != nil {
			fail(err)
			continue
		}
		pl, builder, err := find(ctx, cfg.Component)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("stage %s: plugin://%s is not checked: %v", stage.Name(), cfg.Component, err))
			continue
		}
		names, ok := specs[cfg.Component]
		if !ok {
			settings, err := packer.Settings(pl, builder)
			if err != nil {
				fail(err)
				continue
			}
			names = map[string]bool{}
			for _, s := range settings {
				names[s.Name] = true
			}
			specs[cfg.Component] = names
		}
		if names[plan.DangoKey] {
			warnings = append(warnings, fmt.Sprintf("stage %s: plugin://%s has a setting %q, which %s keeps for opendango's options: set it as a FROM flag", stage.Name(), cfg.Component, plan.DangoKey, plan.SettingsFile))
		}
		if unknown := unknownFlags(cfg.Component, flags, stage, names); len(unknown) > 0 {
			for _, err := range unknown {
				fail(err)
			}
			continue
		}

		// The parent's artifact does not exist yet: placeholders, which
		// Prepare does not look up.
		var parent *packerTemplate
		tableSource := ""
		if stage.Parent != nil {
			name := stage.Parent.Name()
			parent = &packerTemplate{&packer.Artifact{ID: "<artifact of stage " + name + ">", Files: []string{"<file of stage " + name + ">"}}}
			if stage.ParentFlags == nil {
				tableSource = parent.ID()
			}
		}
		settings, err := stageSettings(p.File, stage, cfg, tableSource, contextDir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if parent != nil {
			if err := parentSettings(settings, parent, stage, cfg.Component); err != nil {
				fail(err)
				continue
			}
		}
		pw, err := packer.Prepare(pl, builder, contextDir, settings, coreSettings(stage, cfg, Options{}))
		for _, w := range pw {
			warnings = append(warnings, fmt.Sprintf("stage %s: plugin://%s: %s", stage.Name(), cfg.Component, w))
		}
		if err != nil {
			fail(fmt.Errorf("plugin://%s: %w", cfg.Component, err))
		}
	}
	return warnings, errors.Join(errs...)
}

// unknownFlags returns an error for each flag that is not a setting of the
// builder, with the closest setting when there is one.
func unknownFlags(source string, flags map[string]string, stage *plan.Stage, settings map[string]bool) []error {
	all := make([]string, 0, len(settings))
	for name := range settings {
		all = append(all, name)
	}
	var unknown []string
	for name := range flags {
		if !settings[name] {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	var errs []error
	for _, name := range unknown {
		// Where it comes from: a FROM flag, or dango.yaml.
		where := ""
		if _, ok := stage.From.Flags[name]; !ok {
			if _, ok := stage.FileFlags[name]; ok {
				where = " (in " + plan.SettingsFile + ")"
			}
		}
		msg := fmt.Sprintf("plugin://%s has no setting %q%s (opendango plugin describe %s lists them)", source, name, where, source)
		if s := didyoumean.NameSuggestion(name, all); s != "" {
			msg = fmt.Sprintf("plugin://%s has no setting %q%s, did you mean %s?", source, name, where, s)
		}
		errs = append(errs, errors.New(msg))
	}
	return errs
}
