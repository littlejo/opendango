package packer

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Known is what opendango knows of a Packer builder: the settings that
// OUTPUT and LABEL become, as they do with a native builder. A plugin://
// stage of a builder that is not in the table cannot use them: its plugin
// configuration (FROM flags) sets its artifact.
type Known struct {
	// TemplateName is the setting that OUTPUT --as=template <name> sets.
	TemplateName string
	// Tags is the list setting that the labels go to, as key=value.
	Tags string
	// XVA is the post-processor of the plugin that runs OUTPUT --format=xva
	// on the artifact (or on a template found in the cache), nil when the
	// builder cannot export an XVA.
	XVA *XVAExport

	// HTTPIP is the setting that SERVE --addr sets: the address the
	// machine reaches the plugin's HTTP server at. Empty when the builder
	// has none (SERVE --addr is then an error).
	HTTPIP string
	// BootWait is the setting of the wait before boot_command: with BOOT,
	// opendango sets it to none, as a native stage types the keys once the
	// VM has started (a <waitX> in the keys waits).
	BootWait string

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

	// DropInheritedTags is the list setting of the tag prefixes that the
	// builder removes from the tags the machine copies from its source:
	// opendango sets it to the cache tags, which belong to the source only.
	DropInheritedTags string
	// Cache is the data source that finds a template by its tags, nil when
	// the stages of this builder are not cached.
	Cache *CacheLookup
}

// XVAExport is a post-processor of the plugin that exports the artifact
// (a template) as an XVA, once per OUTPUT --format=xva.
type XVAExport struct {
	// PostProcessor is its name in the plugin.
	PostProcessor string
	// Output and Compress are the settings of the path and of the
	// compression.
	Output, Compress string
	// Copy are the stage's FROM flags passed as they are.
	Copy []string
}

// CacheLookup is a data source of the plugin that finds the template
// tagged with a cache key.
type CacheLookup struct {
	// Datasource is its name in the plugin.
	Datasource string
	// Tags is the list setting of the tags the template must have.
	Tags string
	// SamePoolAs is the setting that restricts the lookup to where a
	// machine created from the stage's source would be.
	SamePoolAs string
	// AllowMissing is the boolean setting that makes "not found" an empty
	// ID rather than an error.
	AllowMissing string
	// Copy are the stage's FROM flags passed as they are (how to reach the
	// infrastructure, where to look).
	Copy []string
	// ID and Name are the attributes of its output.
	ID, Name string
}

// known lists the builders by source type.
var known = map[string]*Known{
	// github.com/littlejo/packer-plugin-xenorchestra: the artifact is a
	// template.
	"xenorchestra": {
		TemplateName: "template_name",
		Tags:         "tags",
		XVA: &XVAExport{
			PostProcessor: "xva",
			Output:        "output",
			Compress:      "compress",
			Copy:          []string{"endpoint", "token", "insecure"},
		},
		HTTPIP:    "http_ip",
		BootWait:  "boot_wait",
		Source:    "template",
		RootOnly:  []string{"template", "pool", "iso"},
		Inherited: []string{"endpoint", "token", "insecure"},

		DropInheritedTags: "drop_inherited_tags",
		Cache: &CacheLookup{
			Datasource:   "template",
			Tags:         "tags",
			SamePoolAs:   "same_pool_as",
			AllowMissing: "allow_missing",
			Copy:         []string{"endpoint", "token", "insecure", "pool"},
			ID:           "id",
			Name:         "name",
		},
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
	if k.HTTPIP != "" {
		m[k.HTTPIP] = "SERVE --addr=<ip>"
	}
	if k.BootWait != "" {
		m[k.BootWait] = "BOOT (a <waitX> in the keys)"
	}
	if k.DropInheritedTags != "" {
		m[k.DropInheritedTags] = "opendango (the cache tags)"
	}
	return m
}

// checkFlags fails when a FROM flag sets a setting of the table.
func (k *Known) checkFlags(flags map[string]string) error {
	for name, instead := range k.settings() {
		if _, ok := flags[name]; ok {
			return fmt.Errorf("plugin: --%s is set by %s", name, instead)
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
			return nil, fmt.Errorf("plugin: --%s cannot be set when starting from another stage", name)
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
