package packer

import (
	"fmt"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// Datasource runs a data source of a plugin (Configure, then Execute) and
// returns the attributes of its output object as text: strings as they
// are, numbers and booleans as written in HCL, lists, maps and objects as
// JSON. Null and unknown attributes are left out.
func Datasource(pl *Plugin, name, dir string, configs ...map[string]any) (map[string]string, error) {
	p, err := start(pl, "datasource", name, dir)
	if err != nil {
		return nil, err
	}
	defer p.stop()
	ds := p.client.Datasource()
	raws := make([]any, len(configs))
	for i, c := range configs {
		raws[i] = c
	}
	if err := ds.Configure(raws...); err != nil {
		return nil, fmt.Errorf("plugin: %s datasource %s: invalid configuration: %w%s", pl.Name, name, err, p.logTail())
	}
	v, err := ds.Execute()
	if err != nil {
		return nil, fmt.Errorf("plugin: %s datasource %s: %w%s", pl.Name, name, err, p.logTail())
	}
	if v.IsNull() || !v.Type().IsObjectType() {
		return nil, fmt.Errorf("plugin: %s datasource %s: the output is not an object", pl.Name, name)
	}
	out := map[string]string{}
	for k, attr := range v.AsValueMap() {
		if !attr.IsWhollyKnown() || attr.IsNull() {
			continue
		}
		switch attr.Type() {
		case cty.String:
			out[k] = attr.AsString()
		case cty.Number:
			out[k] = attr.AsBigFloat().Text('f', -1)
		case cty.Bool:
			out[k] = fmt.Sprint(attr.True())
		default:
			b, err := ctyjson.Marshal(attr, attr.Type())
			if err != nil {
				return nil, fmt.Errorf("plugin: %s datasource %s: output %s: %w", pl.Name, name, k, err)
			}
			out[k] = string(b)
		}
	}
	return out, nil
}
