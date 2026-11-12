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

// PostOutput is what a post-processor made. Its plugin keeps running until
// Close, so that Destroy can delete the artifact.
type PostOutput struct {
	// Made is false when the post-processor made no artifact.
	Made      bool
	ID        string
	BuilderID string
	Files     []string
	// Keep is the post-processor's choice for its input artifact (Packer's
	// keep_input_artifact), ForceOverride tells that it cannot be changed.
	Keep, ForceOverride bool

	a packersdk.Artifact
	p *process
}

// Destroy deletes the artifact through the plugin that made it.
func (o *PostOutput) Destroy() error {
	if o.a == nil {
		return nil
	}
	return o.a.Destroy()
}

// Close stops the plugin.
func (o *PostOutput) Close() {
	if o.p != nil {
		o.p.stop()
	}
}

// PostProcess runs a post-processor of a plugin on an artifact. The caller
// closes the output.
func PostProcess(ctx context.Context, pl *Plugin, name, dir string, ui packersdk.Ui, in Input, configs ...map[string]any) (*PostOutput, error) {
	p, err := start(pl, "post-processor", name, dir)
	if err != nil {
		return nil, err
	}
	pp := p.client.PostProcessor()
	raws := make([]any, len(configs))
	for i, c := range configs {
		raws[i] = c
	}
	if err := pp.Configure(raws...); err != nil {
		p.stop()
		return nil, fmt.Errorf("plugin: %s post-processor %s: invalid configuration: %w%s", pl.Name, name, err, p.logTail())
	}
	out, keep, force, err := pp.PostProcess(ctx, ui, &inputArtifact{in})
	if err != nil {
		p.stop()
		return nil, fmt.Errorf("plugin: %s post-processor %s: %w%s", pl.Name, name, err, p.logTail())
	}
	o := &PostOutput{Keep: keep, ForceOverride: force, p: p}
	if out != nil {
		o.Made, o.a = true, out
		o.ID, o.BuilderID, o.Files = out.Id(), out.BuilderId(), out.Files()
	}
	return o, nil
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
