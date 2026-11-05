package packer

import (
	"context"
	"fmt"

	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
)

// Provision runs a provisioner of a plugin: Prepare with the settings, then
// Provision with the communicator of the machine and the data of the
// connection (Host, Port, User, SSHPrivateKey…, Packer's generated data).
func Provision(ctx context.Context, pl *Plugin, name, dir string, ui packersdk.Ui, comm packersdk.Communicator, data map[string]any, configs ...map[string]any) error {
	p, err := start(pl, "provisioner", name, dir)
	if err != nil {
		return err
	}
	defer p.stop()
	pr := p.client.Provisioner()
	raws := make([]any, len(configs))
	for i, c := range configs {
		raws[i] = c
	}
	if err := pr.Prepare(raws...); err != nil {
		return fmt.Errorf("plugin: %s provisioner %s: invalid configuration: %w%s", pl.Name, name, err, p.logTail())
	}
	if err := pr.Provision(ctx, ui, comm, data); err != nil {
		return fmt.Errorf("plugin: %s provisioner %s: %w%s", pl.Name, name, err, p.logTail())
	}
	return nil
}

// PrepareProvisioner starts the plugin and runs the provisioner's Prepare,
// which checks its settings.
func PrepareProvisioner(pl *Plugin, name, dir string, configs ...map[string]any) error {
	p, err := start(pl, "provisioner", name, dir)
	if err != nil {
		return err
	}
	defer p.stop()
	raws := make([]any, len(configs))
	for i, c := range configs {
		raws[i] = c
	}
	return p.client.Provisioner().Prepare(raws...)
}
