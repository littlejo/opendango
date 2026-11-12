package packer

import (
	"fmt"
	"sort"

	"github.com/hashicorp/hcl/v2/hcldec"
)

// Setting is a setting of a builder: its name and its type, as the plugin
// describes them (HCL2 spec). Plugins do not describe what a setting does,
// nor whether it is required: their Prepare tells.
type Setting struct {
	Name string
	Type string
}

// Settings returns the settings of a builder of the plugin, sorted by name.
func Settings(pl *Plugin, builder string) (_ []Setting, err error) {
	p, err := start(pl, "builder", builder, ".")
	if err != nil {
		return nil, err
	}
	defer p.stop()
	// The SDK's client panics when the plugin sends a spec it cannot
	// decode.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("plugin: %s: reading the settings of %s: %v%s", pl.Name, builder, r, p.logTail())
		}
	}()
	spec := p.client.Builder().ConfigSpec()
	settings := make([]Setting, 0, len(spec))
	for name, s := range spec {
		settings = append(settings, Setting{Name: name, Type: specType(s)})
	}
	sort.Slice(settings, func(i, j int) bool { return settings[i].Name < settings[j].Name })
	return settings, nil
}

func specType(s hcldec.Spec) string {
	switch s := s.(type) {
	case *hcldec.AttrSpec:
		return s.Type.FriendlyName()
	case *hcldec.BlockSpec:
		return "block"
	case *hcldec.BlockListSpec, *hcldec.BlockSetSpec:
		return "list of blocks"
	case *hcldec.BlockAttrsSpec:
		return "map of " + s.ElementType.FriendlyName()
	case *hcldec.BlockMapSpec:
		return "map of blocks"
	default:
		return fmt.Sprintf("%T", s)
	}
}

// Prepare starts the plugin and runs the builder's Prepare: the plugin
// checks the configuration without creating anything (Packer's contract).
// It returns the plugin's warnings.
func Prepare(pl *Plugin, builder, dir string, configs ...map[string]any) ([]string, error) {
	p, err := start(pl, "builder", builder, dir)
	if err != nil {
		return nil, err
	}
	defer p.stop()
	raws := make([]any, len(configs))
	for i, c := range configs {
		raws[i] = c
	}
	_, warnings, err := p.client.Builder().Prepare(raws...)
	return warnings, err
}

// Reserved tells which instruction sets a setting of the source type, when
// opendango sets it: it is not a FROM flag.
func Reserved(source, name string) string {
	if instead, ok := reserved[name]; ok {
		return instead
	}
	if k := Lookup(source); k != nil {
		return k.settings()[name]
	}
	return ""
}

// DocURL returns the documentation page of a plugin installed in Packer's
// layout (github.com/<owner>/<name>/): developer.hashicorp.com for the
// official plugins, the GitHub repository for the others; "" otherwise.
func DocURL(pl *Plugin) string {
	src, ok := SourceOf(pl)
	switch {
	case !ok:
		return ""
	case src.Owner == "hashicorp":
		return "https://developer.hashicorp.com/packer/integrations/hashicorp/" + pl.Name
	default:
		return "https://github.com/" + src.repo()
	}
}
