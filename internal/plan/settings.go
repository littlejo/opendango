package plan

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"

	"go.yaml.in/yaml/v3"

	"github.com/littlejo/opendango/builders/packer"
)

// SettingsFile is the name of the file of plugin settings read next to the
// Dangofile.
const SettingsFile = "dango.yaml"

// Settings are the plugin settings of a dango.yaml file, by source type
// (qemu, xenserver-iso): the values as FROM flags would give them (scalars
// as strings, lists and maps as JSON). The FROM flags of a stage take
// precedence.
type Settings struct {
	// File is the path of the file, for the messages.
	File     string
	Sections map[string]map[string]string
}

// LoadSettings reads a dango.yaml file:
//
//	qemu:
//	  disk_size: 10G
//	  memory: 2048
//	  cd_files: [cidata/user-data, cidata/meta-data]
//
// Secrets (settings named *password*, *secret*, *token*) are refused: they
// come from the environment, through ARG and --build-arg.
func LoadSettings(path string) (*Settings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s := &Settings{File: path, Sections: map[string]map[string]string{}}
	var sources []string
	for source := range raw {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	var errs []error
	for _, source := range sources {
		section := map[string]string{}
		for name, v := range raw[source] {
			if packer.Sensitive(name) {
				errs = append(errs, fmt.Errorf("%s: %s.%s: no secret in %s: use ARG and --build-arg in the FROM line", path, source, name, SettingsFile))
				continue
			}
			value, err := flagValue(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %s.%s: %w", path, source, name, err))
				continue
			}
			section[name] = value
		}
		// The checks of the FROM flags: the source type, the settings that
		// instructions set, the JSON values.
		if _, err := packer.ParseConfig(source, section); err != nil {
			errs = append(errs, fmt.Errorf("%s: %s: %w", path, source, err))
			continue
		}
		s.Sections[source] = section
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return s, nil
}

// flagValue writes a YAML value as a FROM flag would give it.
func flagValue(v any) (string, error) {
	switch v := v.(type) {
	case nil:
		return "", errors.New("no value: remove the setting, or give it one")
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case int:
		return strconv.Itoa(v), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case []any, map[string]any:
		b, err := json.Marshal(v)
		return string(b), err
	default:
		return "", fmt.Errorf("unsupported value %v (%T)", v, v)
	}
}
