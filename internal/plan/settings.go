package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// Dango are the options of opendango by source type: the "dango" key
	// of a section, apart from the plugin settings.
	Dango map[string]DangoOptions
}

// DangoKey is the key of a section that holds opendango's options rather
// than plugin settings.
const DangoKey = "dango"

// DangoOptions are the options of opendango in a dango.yaml section.
type DangoOptions struct {
	// Connect is the default CONNECT of the stages of the source, with the
	// syntax of the instruction (ssh --user=debian); a CONNECT in the
	// Dangofile takes precedence.
	Connect string `yaml:"connect"`
	// Parent are the settings of a stage that starts from another one (FROM
	// <stage>), over the section: how the plugin starts from the parent's
	// artifact, with ${DANGO_PARENT_ID} and ${DANGO_PARENT_FILE}. For a
	// builder of the table, they replace its own way.
	Parent map[string]string `yaml:"-"`
	// ParentRaw is Parent as read.
	ParentRaw map[string]any `yaml:"parent"`
}

// The variables of the parent's artifact in dango.parent, replaced at build
// time: markers until then (plain text: they may sit in JSON values).
const (
	ParentIDVar      = "DANGO_PARENT_ID"
	ParentFileVar    = "DANGO_PARENT_FILE"
	ParentIDMarker   = "@@DANGO_PARENT_ID@@"
	ParentFileMarker = "@@DANGO_PARENT_FILE@@"
)

// LoadSettings reads a dango.yaml file:
//
//	qemu:
//	  dango:
//	    connect: ssh --user=debian
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
	s := &Settings{File: path, Sections: map[string]map[string]string{}, Dango: map[string]DangoOptions{}}
	var sources []string
	for source := range raw {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	var errs []error
	for _, source := range sources {
		section := map[string]string{}
		if v, ok := raw[source][DangoKey]; ok {
			opts, err := dangoOptions(v)
			if err == nil && opts.Parent != nil {
				_, err = packer.ParseConfig(source, opts.Parent)
				if err != nil {
					err = fmt.Errorf("parent: %w", err)
				}
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %s.%s: %w", path, source, DangoKey, err))
			}
			s.Dango[source] = opts
		}
		for name, v := range raw[source] {
			if name == DangoKey {
				continue
			}
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

// dangoOptions reads the "dango" key of a section; an unknown option is an
// error.
func dangoOptions(v any) (DangoOptions, error) {
	var opts DangoOptions
	b, err := yaml.Marshal(v)
	if err != nil {
		return opts, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&opts); err != nil && !errors.Is(err, io.EOF) {
		return opts, fmt.Errorf("%w (options: connect, parent)", err)
	}
	if opts.ParentRaw != nil {
		opts.Parent = map[string]string{}
		for name, v := range opts.ParentRaw {
			if packer.Sensitive(name) {
				return opts, fmt.Errorf("parent.%s: no secret in %s: use ARG and --build-arg in the FROM line", name, SettingsFile)
			}
			value, err := flagValue(v)
			if err != nil {
				return opts, fmt.Errorf("parent.%s: %w", name, err)
			}
			opts.Parent[name] = value
		}
	}
	return opts, nil
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
