package packer

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Known is what opendango knows of a Packer builder: the settings that
// OUTPUT and LABEL become, as they do with a native builder. A packer://
// stage of a builder that is not in the table cannot use them: its plugin
// configuration (FROM flags) sets its artifact.
type Known struct {
	// TemplateName is the setting that OUTPUT --as=template <name> sets.
	TemplateName string
	// Tags is the list setting that the labels go to, as key=value.
	Tags string
	// XVAOutput and XVACompress are the settings of OUTPUT --format=xva
	// [--compress] <path>, empty when the builder cannot export an XVA.
	// The builder exports one XVA at most.
	XVAOutput, XVACompress string

	// Source is the setting that FROM <stage> sets to the id of the parent
	// stage's artifact, empty when a stage cannot start from this builder's
	// artifact.
	Source string
	// RootOnly are the settings that only a root stage sets: they choose
	// what the VM starts from, which FROM <stage> does.
	RootOnly []string
	// Inherited are the settings a stage takes from its parent's FROM line
	// when it does not set them: how to reach the infrastructure.
	Inherited []string
}

// known lists the builders by source type.
var known = map[string]*Known{
	// github.com/littlejo/packer-plugin-xenorchestra: the artifact is a
	// template.
	"xenorchestra": {
		TemplateName: "template_name",
		Tags:         "tags",
		XVAOutput:    "xva_output",
		XVACompress:  "xva_compress",
		Source:       "template",
		RootOnly:     []string{"template", "pool", "iso"},
		Inherited:    []string{"endpoint", "token", "insecure"},
	},
}

// Lookup returns what opendango knows of the builder of a source type, or
// nil.
func Lookup(source string) *Known { return known[source] }

// KnownSources lists the source types of the table.
func KnownSources() string {
	var names []string
	for name := range known {
		names = append(names, name)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// settings returns the settings that the table maps, and which instruction
// sets each of them: they cannot be FROM flags.
func (k *Known) settings() map[string]string {
	m := map[string]string{}
	if k.TemplateName != "" {
		m[k.TemplateName] = "OUTPUT --as=template <name>"
	}
	if k.Tags != "" {
		m[k.Tags] = "LABEL"
	}
	if k.XVAOutput != "" {
		m[k.XVAOutput] = "OUTPUT --format=xva <path>"
	}
	if k.XVACompress != "" {
		m[k.XVACompress] = "OUTPUT --format=xva --compress=<algorithm>"
	}
	return m
}

// checkFlags fails when a FROM flag sets a setting of the table.
func (k *Known) checkFlags(flags map[string]string) error {
	for name, instead := range k.settings() {
		if _, ok := flags[name]; ok {
			return fmt.Errorf("packer: --%s is set by %s", name, instead)
		}
	}
	return nil
}

// StageFlags returns the FROM flags of a stage that starts from another
// one (FROM [--flags] <stage>): its own flags, plus the Inherited flags of
// the parent it does not set. parent are the parent's flags, with its own
// inherited ones.
func (k *Known) StageFlags(parent, own map[string]string) (map[string]string, error) {
	if k.Source == "" {
		return nil, errors.New("its artifact cannot be the source of another stage")
	}
	for _, name := range append([]string{k.Source}, k.RootOnly...) {
		if _, ok := own[name]; ok {
			return nil, fmt.Errorf("packer: --%s cannot be set when starting from another stage", name)
		}
	}
	flags := map[string]string{}
	for _, name := range k.Inherited {
		if v, ok := parent[name]; ok {
			flags[name] = v
		}
	}
	for name, v := range own {
		flags[name] = v
	}
	return flags, nil
}
