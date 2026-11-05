package packer

import (
	"context"
	"fmt"

	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
)

// PostProcess runs a post-processor of a plugin on the artifact id (such
// as a template UUID, possibly found in the cache) and returns the files of
// its artifact.
func PostProcess(ctx context.Context, pl *Plugin, name, dir string, ui packersdk.Ui, id string, configs ...map[string]any) ([]string, error) {
	p, err := start(pl, "post-processor", name, dir)
	if err != nil {
		return nil, err
	}
	defer p.stop()
	pp := p.client.PostProcessor()
	raws := make([]any, len(configs))
	for i, c := range configs {
		raws[i] = c
	}
	if err := pp.Configure(raws...); err != nil {
		return nil, fmt.Errorf("plugin: %s post-processor %s: invalid configuration: %w%s", pl.Name, name, err, p.logTail())
	}
	out, _, _, err := pp.PostProcess(ctx, ui, &inputArtifact{id: id})
	if err != nil {
		return nil, fmt.Errorf("plugin: %s post-processor %s: %w%s", pl.Name, name, err, p.logTail())
	}
	if out == nil {
		return nil, nil
	}
	return out.Files(), nil
}

// inputArtifact is the artifact given to a post-processor: only its id is
// known.
type inputArtifact struct{ id string }

func (a *inputArtifact) BuilderId() string { return "opendango" }
func (a *inputArtifact) Files() []string   { return nil }
func (a *inputArtifact) Id() string        { return a.id }
func (a *inputArtifact) String() string    { return a.id }
func (a *inputArtifact) State(string) any  { return nil }
func (a *inputArtifact) Destroy() error    { return nil }
