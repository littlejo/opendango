package packer

import (
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
