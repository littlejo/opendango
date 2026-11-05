package packer

import (
	"context"
	"fmt"

	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
)

// Input is the artifact a post-processor gets: what a build made, or a
// template found in the cache.
type Input struct {
	ID        string
	BuilderID string
	Files     []string
}

// PostProcess runs a post-processor of a plugin on an artifact and returns
// the files of its own artifact.
func PostProcess(ctx context.Context, pl *Plugin, name, dir string, ui packersdk.Ui, in Input, configs ...map[string]any) ([]string, error) {
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
	out, _, _, err := pp.PostProcess(ctx, ui, &inputArtifact{in})
	if err != nil {
		return nil, fmt.Errorf("plugin: %s post-processor %s: %w%s", pl.Name, name, err, p.logTail())
	}
	if out == nil {
		return nil, nil
	}
	return out.Files(), nil
}

// inputArtifact is the artifact given to a post-processor.
type inputArtifact struct{ Input }

func (a *inputArtifact) BuilderId() string {
	if a.BuilderID == "" {
		return "opendango"
	}
	return a.BuilderID
}
func (a *inputArtifact) Files() []string  { return a.Input.Files }
func (a *inputArtifact) Id() string       { return a.ID }
func (a *inputArtifact) String() string   { return a.ID }
func (a *inputArtifact) State(string) any { return nil }
func (a *inputArtifact) Destroy() error   { return nil }

// ConfigurePostProcessor starts the plugin and runs the post-processor's
// Configure, which checks its settings without doing anything.
func ConfigurePostProcessor(pl *Plugin, name, dir string, configs ...map[string]any) error {
	p, err := start(pl, "post-processor", name, dir)
	if err != nil {
		return err
	}
	defer p.stop()
	raws := make([]any, len(configs))
	for i, c := range configs {
		raws[i] = c
	}
	return p.client.PostProcessor().Configure(raws...)
}
