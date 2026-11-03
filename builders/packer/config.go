// Package packer runs the builders of Packer plugins (packer-plugin-*
// binaries) through packer-plugin-sdk: FROM packer://<plugin>-<builder>.
//
// The plugin drives the build: it creates the machine, connects to it, and
// calls the provision hook with its communicator; that is when opendango
// runs the instructions of the stage. Then the plugin stops the machine and
// produces its artifact.
//
// No code comes from Packer itself (BSL): only the SDK (MPL-2.0) is used.
package packer

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Config is the configuration of a packer:// stage.
type Config struct {
	// Component is the Packer source type, such as xenserver-iso.
	Component string
	// Settings is the plugin configuration, from the FROM flags: strings,
	// or JSON values for the flags that start with [ or {.
	Settings map[string]any
	// Secrets are the values of the settings whose name looks sensitive
	// (password, secret, token): masked in the log, and passed to the
	// plugin as packer_sensitive_variables so that it masks them too.
	Secrets []string
}

var (
	component = regexp.MustCompile(`^[a-z0-9]+([-_][a-z0-9]+)*$`)
	sensitive = regexp.MustCompile(`password|secret|token`)
)

// reserved are the settings that come from CONNECT and SERVE, not from
// FROM flags.
var reserved = map[string]string{
	"communicator":   "CONNECT ssh",
	"ssh_username":   "CONNECT ssh --user=<user>",
	"ssh_port":       "CONNECT ssh --port=<port>",
	"ssh_timeout":    "CONNECT ssh --timeout=<duration>",
	"http_directory": "SERVE <dir>",
	"http_port_min":  "SERVE --port=<port>",
	"http_port_max":  "SERVE --port=<port>",
}

// ParseConfig reads FROM [--flags] packer://<source>. Flag names are the
// plugin's configuration keys, unchanged (--vm_name=golden).
func ParseConfig(source string, flags map[string]string) (*Config, error) {
	if !component.MatchString(source) {
		return nil, fmt.Errorf("packer: invalid source %q: expected <plugin>-<builder>, such as xenserver-iso", source)
	}
	cfg := &Config{Component: source, Settings: map[string]any{}}
	for k, v := range flags {
		if instead, ok := reserved[k]; ok {
			return nil, fmt.Errorf("packer: --%s is set by %s", k, instead)
		}
		if strings.HasPrefix(k, "packer_") {
			return nil, fmt.Errorf("packer: --%s is reserved to opendango", k)
		}
		if sensitive.MatchString(k) && v != "" {
			cfg.Secrets = append(cfg.Secrets, v)
		}
		if strings.HasPrefix(v, "[") || strings.HasPrefix(v, "{") {
			var value any
			if err := json.Unmarshal([]byte(v), &value); err != nil {
				return nil, fmt.Errorf("packer: --%s: invalid JSON: %w", k, err)
			}
			cfg.Settings[k] = value
			continue
		}
		cfg.Settings[k] = v
	}
	return cfg, nil
}
