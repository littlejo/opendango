package xo

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/littlejo/opendango/internal/instructions"
)

// Builder creates a VM from an XO template and drives it.
type Builder struct {
	cfg    Config
	client *Client
	name   string
	log    func(string)
	vmID   string
	// converted is true once the VM is a template.
	converted bool
	// pollInterval is the pause between two state checks.
	pollInterval time.Duration
}

// NewBuilder returns a builder that names its VM name.
func NewBuilder(cfg Config, client *Client, name string, log func(string)) *Builder {
	return &Builder{cfg: cfg, client: client, name: name, log: log, pollInterval: 5 * time.Second}
}

// ID returns the VM UUID, or its name before creation.
func (b *Builder) ID() string {
	if b.vmID == "" {
		return b.name
	}
	return b.vmID
}

// Start resolves the template, pool, SR and network, then creates and boots
// the VM with the cloud-config.
func (b *Builder) Start(ctx context.Context, cloudConfig string) error {
	poolID := ""
	if b.cfg.Pool != "" {
		pool, err := b.find(ctx, "pools", "pool", b.cfg.Pool, "")
		if err != nil {
			return err
		}
		poolID = pool.ID
	}
	// The same template UUID can exist in several pools (XO Hub
	// templates): restrict to --pool when set.
	tmpl, err := b.find(ctx, "vm-templates", "template", b.cfg.Template, poolID)
	if err != nil {
		return err
	}
	poolID = tmpl.Pool

	body := map[string]any{
		"name_label":       b.name,
		"name_description": "Built by opendango",
		"template":         tmpl.UUID,
		"boot":             true,
	}
	if b.cfg.CPUs > 0 {
		body["cpus"] = b.cfg.CPUs
	}
	if b.cfg.Memory > 0 {
		body["memory"] = b.cfg.Memory
	}
	if b.cfg.Disk > 0 || b.cfg.SR != "" {
		vdi := map[string]any{"userdevice": "0"}
		if b.cfg.Disk > 0 {
			vdi["size"] = b.cfg.Disk
		}
		if b.cfg.SR != "" {
			sr, err := b.find(ctx, "srs", "SR", b.cfg.SR, poolID)
			if err != nil {
				return err
			}
			vdi["sr"] = sr.ID
		}
		body["vdis"] = []any{vdi}
	}
	if b.cfg.Network != "" {
		network, err := b.find(ctx, "networks", "network", b.cfg.Network, poolID)
		if err != nil {
			return err
		}
		body["vifs"] = []any{map[string]any{"device": "0", "network": network.ID}}
	}
	if cloudConfig != "" {
		body["cloud_config"] = cloudConfig
		// The cloud-config holds the temporary SSH key: do not leave it
		// attached to the image.
		body["destroy_cloud_config_vdi"] = true
	}

	b.log(fmt.Sprintf("Creating VM %s from template %s", b.name, tmpl.NameLabel))
	var created struct {
		ID string `json:"id"`
	}
	if err := b.client.action(ctx, "/pools/"+poolID, "create_vm", body, &created); err != nil {
		return err
	}
	if created.ID == "" {
		return fmt.Errorf("xo: create_vm returned no VM id")
	}
	b.vmID = created.ID
	return nil
}

// vm is the subset of VM fields read by the builder.
type vm struct {
	PowerState    string            `json:"power_state"`
	MainIPAddress string            `json:"mainIpAddress"`
	Addresses     map[string]string `json:"addresses"`
}

func (b *Builder) getVM(ctx context.Context) (*vm, error) {
	var v vm
	err := b.client.do(ctx, http.MethodGet, "/vms/"+b.vmID, nil, &v)
	return &v, err
}

// Address waits until the guest tools report an IPv4 address. Without
// DHCP on the VM network, XO only reports an IPv6 link-local address.
func (b *Builder) Address(ctx context.Context) (string, error) {
	for {
		v, err := b.getVM(ctx)
		if err != nil {
			return "", err
		}
		if ip := ipv4(v); ip != "" {
			return ip, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("xo: no IPv4 address reported for VM %s: the template needs the guest tools, and the VM network needs DHCP (set --network)", b.vmID)
		case <-time.After(b.pollInterval):
		}
	}
}

// ipv4 prefers mainIpAddress, then the first IPv4 of addresses
// ("<device>/ipv4/<n>" keys).
func ipv4(v *vm) string {
	if v.MainIPAddress != "" && !strings.Contains(v.MainIPAddress, ":") {
		return v.MainIPAddress
	}
	keys := make([]string, 0, len(v.Addresses))
	for k := range v.Addresses {
		if strings.Contains(k, "/ipv4/") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return v.Addresses[keys[0]]
}

// Stop waits up to grace for the VM to halt, then asks for a clean shutdown,
// then forces it.
func (b *Builder) Stop(ctx context.Context, grace time.Duration) error {
	if halted, err := b.waitHalted(ctx, grace); err != nil || halted {
		return err
	}
	b.log("Asking for a clean shutdown")
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	err := b.client.action(cctx, "/vms/"+b.vmID, "clean_shutdown", nil, nil)
	cancel()
	if err == nil {
		return nil
	}
	b.log(fmt.Sprintf("Clean shutdown failed (%v), forcing it", err))
	return b.client.action(ctx, "/vms/"+b.vmID, "hard_shutdown", nil, nil)
}

func (b *Builder) waitHalted(ctx context.Context, grace time.Duration) (bool, error) {
	deadline := time.Now().Add(grace)
	for {
		v, err := b.getVM(ctx)
		if err != nil {
			return false, err
		}
		if v.PowerState == "Halted" {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(b.pollInterval):
		}
	}
}

// Output exports the VM as XVA or converts it to a template.
func (b *Builder) Output(ctx context.Context, out *instructions.Output) error {
	switch out.Kind {
	case instructions.OutputXVA:
		return b.exportXVA(ctx, out.Path, out.Compress)
	case instructions.OutputTemplate:
		if out.Name != "" {
			if err := b.client.do(ctx, http.MethodPatch, "/vms/"+b.vmID, map[string]string{"nameLabel": out.Name}, nil); err != nil {
				return err
			}
		}
		if err := b.client.convertToTemplate(ctx, b.vmID); err != nil {
			return err
		}
		b.converted = true
		return b.waitTemplate(ctx)
	default:
		return fmt.Errorf("xo: unsupported output %q", out.Kind)
	}
}

// waitTemplate waits until XO lists the converted VM as a template:
// vm.convertToTemplate returns before XO's object cache is updated.
func (b *Builder) waitTemplate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		err := b.client.do(ctx, http.MethodGet, "/vm-templates/"+b.vmID, nil, nil)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("xo: VM %s converted but not listed as a template yet: %w", b.vmID, err)
		case <-time.After(b.pollInterval):
		}
	}
}

// exportXVA streams the export to a temporary file renamed on success.
func (b *Builder) exportXVA(ctx context.Context, dest, compress string) error {
	p := "/vms/" + b.vmID + ".xva"
	if compress != "" {
		p += "?compress=" + url.QueryEscape(compress)
	}
	resp, err := b.client.send(ctx, http.MethodGet, p, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".opendango-*.xva")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("xo: downloading the XVA: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

// Destroy deletes the template the VM became, or forces the VM off if
// needed and deletes it, with its disks.
func (b *Builder) Destroy(ctx context.Context) error {
	if b.vmID == "" {
		return nil
	}
	if b.converted {
		return b.client.do(ctx, http.MethodDelete, "/vm-templates/"+b.vmID, nil, nil)
	}
	v, err := b.getVM(ctx)
	if err != nil {
		return err
	}
	if v.PowerState != "Halted" {
		if err := b.client.action(ctx, "/vms/"+b.vmID, "hard_shutdown", nil, nil); err != nil {
			return err
		}
	}
	return b.client.do(ctx, http.MethodDelete, "/vms/"+b.vmID, nil, nil)
}

// find returns the object of collection whose UUID, id or name is ref,
// restricted to poolID when set.
func (b *Builder) find(ctx context.Context, collection, kind, ref, poolID string) (*object, error) {
	objs, err := b.client.list(ctx, collection)
	if err != nil {
		return nil, err
	}
	return match(objs, kind, ref, poolID)
}

// match picks the single object matching ref (UUID, id, or name when no
// UUID or id matches) in poolID (any pool when empty).
func match(objs []object, kind, ref, poolID string) (*object, error) {
	var byID, byName []object
	for _, o := range objs {
		if poolID != "" && o.Pool != poolID {
			continue
		}
		switch {
		case o.UUID == ref || o.ID == ref:
			byID = append(byID, o)
		case o.NameLabel == ref:
			byName = append(byName, o)
		}
	}
	found := byID
	if len(found) == 0 {
		found = byName
	}
	switch {
	case len(found) == 1:
		return &found[0], nil
	case len(found) == 0 && poolID != "":
		return nil, fmt.Errorf("xo: no %s %q in pool %s", kind, ref, poolID)
	case len(found) == 0:
		return nil, fmt.Errorf("xo: no %s %q", kind, ref)
	}
	pools := map[string]bool{}
	for _, o := range found {
		pools[o.Pool] = true
	}
	if len(pools) > 1 {
		return nil, fmt.Errorf("xo: %s %q exists in %d pools, set --pool", kind, ref, len(pools))
	}
	return nil, fmt.Errorf("xo: %d %ss match %q, use the UUID", len(found), kind, ref)
}
