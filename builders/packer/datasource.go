package packer

import (
	"fmt"

	"github.com/zclconf/go-cty/cty"
)

// Datasource runs a data source of a plugin (Configure, then Execute) and
// returns the string attributes of its output object.
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
		if attr.IsKnown() && !attr.IsNull() && attr.Type() == cty.String {
			out[k] = attr.AsString()
		}
	}
	return out, nil
}
