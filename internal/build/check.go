package build

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/hashicorp/packer-plugin-sdk/didyoumean"

	"github.com/littlejo/opendango/builders/packer"
	"github.com/littlejo/opendango/internal/plan"
)

// CheckPlugins checks the FROM flags of the plugin:// stages with their
// plugins, without building: a flag that is not a setting of the builder
// (with the closest one), then the builder's Prepare, which checks the
// configuration without creating anything. A plugin that is not installed
// only gives a warning: the stage is not checked.
func CheckPlugins(ctx context.Context, p *plan.Plan, contextDir string, find FindPluginFunc) (warnings []string, err error) {
	if find == nil {
		find = FindPlugin
	}
	specs := map[string]map[string]bool{}
	var errs []error
	for _, stage := range p.Stages {
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
		if unknown := unknownFlags(cfg.Component, flags, names); len(unknown) > 0 {
			for _, err := range unknown {
				fail(err)
			}
			continue
		}

		// The parent's artifact does not exist yet: a placeholder, which
		// Prepare does not look up.
		parentID := ""
		if stage.Parent != nil {
			parentID = "<template of stage " + stage.Parent.Name() + ">"
		}
		settings, err := stageSettings(p.File, stage, cfg, parentID, contextDir)
		if err != nil {
			errs = append(errs, err)
			continue
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
func unknownFlags(source string, flags map[string]string, settings map[string]bool) []error {
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
		msg := fmt.Sprintf("plugin://%s has no setting %q (opendango plugin describe %s lists them)", source, name, source)
		if s := didyoumean.NameSuggestion(name, all); s != "" {
			msg = fmt.Sprintf("plugin://%s has no setting %q, did you mean --%s?", source, name, s)
		}
		errs = append(errs, errors.New(msg))
	}
	return errs
}
