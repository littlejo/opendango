// Command packer-plugin-opendangotest is a Packer plugin for opendango's
// tests: its "local" builder (source opendangotest-local) provisions the
// machine running the tests, in the directory set by the "root" setting,
// through a communicator that runs /bin/sh locally. It drives opendango
// through the real plugin protocol, without any infrastructure.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/hashicorp/packer-plugin-sdk/common"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/plugin"
	"github.com/hashicorp/packer-plugin-sdk/template/config"
	"github.com/zclconf/go-cty/cty"
)

type Config struct {
	common.PackerConfig `mapstructure:",squash"`

	// Root is the directory the commands run in.
	Root  string   `mapstructure:"root"`
	Count int      `mapstructure:"count"`
	Items []string `mapstructure:"items"`
	// Password is only there to be masked.
	Password string `mapstructure:"password"`
	// Fail makes the build fail after the provision hook.
	Fail bool `mapstructure:"fail"`

	Communicator string `mapstructure:"communicator"`
	SSHUsername  string `mapstructure:"ssh_username"`
	SSHPort      int    `mapstructure:"ssh_port"`
	SSHTimeout   string `mapstructure:"ssh_timeout"`

	HTTPDir     string `mapstructure:"http_directory"`
	HTTPPortMin int    `mapstructure:"http_port_min"`
	HTTPPortMax int    `mapstructure:"http_port_max"`

	BootCommand []string `mapstructure:"boot_command"`

	// The settings opendango maps for plugin://xenorchestra (OUTPUT,
	// LABEL), which the tests point to this plugin.
	TemplateName string   `mapstructure:"template_name"`
	Tags         []string `mapstructure:"tags"`
	XVAOutput    string   `mapstructure:"xva_output"`
	XVACompress  string   `mapstructure:"xva_compress"`
	// Source is the parent's artifact of a stage that starts from another
	// one (FROM <stage>).
	Source   string `mapstructure:"template"`
	HTTPIP   string `mapstructure:"http_ip"`
	BootWait string `mapstructure:"boot_wait"`
	// DropInheritedTags is printed, for the tests.
	DropInheritedTags []string `mapstructure:"drop_inherited_tags"`
}

type Builder struct{ config Config }

func (b *Builder) ConfigSpec() hcldec.ObjectSpec { return hcldec.ObjectSpec{} }

func (b *Builder) Prepare(raws ...any) ([]string, []string, error) {
	if err := config.Decode(&b.config, &config.DecodeOpts{}, raws...); err != nil {
		return nil, nil, err
	}
	if b.config.Root == "" {
		return nil, nil, errors.New("root is required")
	}
	return nil, []string{"this is a test plugin"}, nil
}

func (b *Builder) Run(ctx context.Context, ui packersdk.Ui, hook packersdk.Hook) (packersdk.Artifact, error) {
	c := b.config
	ui.Say(fmt.Sprintf("config: build=%s type=%s on_error=%s count=%d items=%s communicator=%s ssh=%s:%d timeout=%s",
		c.PackerBuildName, c.PackerBuilderType, c.PackerOnError, c.Count, strings.Join(c.Items, ","),
		c.Communicator, c.SSHUsername, c.SSHPort, c.SSHTimeout))
	for _, keys := range c.BootCommand {
		ui.Say("boot_command: " + keys)
	}
	if len(c.DropInheritedTags) > 0 {
		ui.Say("drop_inherited_tags: " + strings.Join(c.DropInheritedTags, ","))
	}
	if c.BootWait != "" {
		ui.Say("boot_wait: " + c.BootWait)
	}
	if c.Source != "" {
		ui.Say("source: template=" + c.Source)
	}
	if c.TemplateName != "" || len(c.Tags) > 0 || c.XVAOutput != "" {
		ui.Say(fmt.Sprintf("artifact: template_name=%s tags=%s xva=%s compress=%s", c.TemplateName, strings.Join(c.Tags, ","), c.XVAOutput, c.XVACompress))
	}
	if c.HTTPDir != "" {
		ui.Say(fmt.Sprintf("http: dir=%s ports=%d-%d", c.HTTPDir, c.HTTPPortMin, c.HTTPPortMax))
	}
	// The generated data of a real builder; PackerHTTPAddr only with an
	// HTTP server, like the SDK's.
	data := map[string]any{"Host": "127.0.0.1", "Port": c.SSHPort, "User": c.SSHUsername, "SSHPrivateKey": "PRIVATE KEY", "PackerHTTPAddr": "ERR_HTTP_ADDR_NOT_IMPLEMENTED_BY_BUILDER"}
	if c.HTTPDir != "" {
		ip := "192.0.2.9"
		if c.HTTPIP != "" {
			ip = c.HTTPIP
		}
		data["PackerHTTPAddr"] = ip + ":8080"
	}
	if err := hook.Run(ctx, packersdk.HookProvision, ui, &comm{root: c.Root}, data); err != nil {
		if c.PackerOnError == "ask" {
			answer, _ := ui.Ask("[c] Clean up and exit, [a] abort without cleanup, or [r] retry step?")
			ui.Say("cleanup answer: " + answer)
		}
		return nil, err
	}
	if c.Fail {
		return nil, errors.New("failing as asked")
	}
	// The tags go to the store of the "template" data source.
	if err := storeTags(c.Tags, c.Root); err != nil {
		return nil, err
	}
	return &artifact{root: c.Root}, nil
}

// comm runs commands with /bin/sh in root.
type comm struct{ root string }

// stubs replace the commands a test must never run for real: sudo runs
// its command as is, reboot and shutdown only log, and cat reads the boot
// ID that reboot changes.
var stubs = map[string]string{
	"sudo":     "while [ $# -gt 0 ] && [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n",
	"reboot":   "echo \"reboot $*\" >> \"$HOME/.stub-log\"\necho \"boot-$(date +%s%N)\" > \"$HOME/.boot_id\"\n",
	"shutdown": "echo \"shutdown $*\" >> \"$HOME/.stub-log\"\n",
	"cat":      "if [ \"$1\" = /proc/sys/kernel/random/boot_id ]; then [ -f \"$HOME/.boot_id\" ] || echo boot-0 > \"$HOME/.boot_id\"; exec /bin/cat \"$HOME/.boot_id\"; fi\nexec /bin/cat \"$@\"\n",
}

// writeStubs writes the stubs to <root>/.stubs, which comes first in PATH.
func writeStubs(root string) error {
	dir := filepath.Join(root, ".stubs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (c *comm) Start(ctx context.Context, rc *packersdk.RemoteCmd) error {
	if err := writeStubs(c.root); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", rc.Command)
	cmd.Dir = c.root
	cmd.Env = append(os.Environ(), "HOME="+c.root, "PATH="+filepath.Join(c.root, ".stubs")+":"+os.Getenv("PATH"))
	cmd.Stdout, cmd.Stderr = rc.Stdout, rc.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		err := cmd.Wait()
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			code = 1
		}
		rc.SetExited(code)
	}()
	return nil
}

func (c *comm) Upload(dst string, r io.Reader, fi *os.FileInfo) error {
	mode := os.FileMode(0o644)
	if fi != nil && *fi != nil {
		mode = (*fi).Mode().Perm()
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (c *comm) UploadDir(dst, src string, _ []string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		return c.Upload(target, f, &info)
	})
}

func (c *comm) Download(string, io.Writer) error           { return errors.New("not supported") }
func (c *comm) DownloadDir(string, string, []string) error { return errors.New("not supported") }

type artifact struct{ root string }

func (a *artifact) BuilderId() string { return "opendango.test" }
func (a *artifact) Files() []string   { return nil }
func (a *artifact) Id() string        { return a.root }
func (a *artifact) String() string    { return "local machine at " + a.root }
func (a *artifact) State(string) any  { return nil }

// Destroy leaves a marker in root, for the tests.
func (a *artifact) Destroy() error {
	return os.WriteFile(filepath.Join(a.root, "artifact-destroyed"), nil, 0o644)
}

// The "template" data source finds an artifact by its tags in a store: the
// directory OPENDANGO_TEST_STORE, with one file per tag that holds the
// artifact id (the builder's root). It stands for the one of
// packer-plugin-xenorchestra.
type dsConfig struct {
	Tags         []string `mapstructure:"tags"`
	SamePoolAs   string   `mapstructure:"same_pool_as"`
	AllowMissing bool     `mapstructure:"allow_missing"`
	Endpoint     string   `mapstructure:"endpoint"`
	Token        string   `mapstructure:"token"`
	Insecure     string   `mapstructure:"insecure"`
	Pool         string   `mapstructure:"pool"`
}

type datasource struct{ config dsConfig }

func (d *datasource) ConfigSpec() hcldec.ObjectSpec { return hcldec.ObjectSpec{} }

func (d *datasource) OutputSpec() hcldec.ObjectSpec {
	return hcldec.ObjectSpec{
		"id":   &hcldec.AttrSpec{Name: "id", Type: cty.String},
		"name": &hcldec.AttrSpec{Name: "name", Type: cty.String},
	}
}

func (d *datasource) Configure(raws ...any) error {
	return config.Decode(&d.config, &config.DecodeOpts{}, raws...)
}

func (d *datasource) Execute() (cty.Value, error) {
	id := ""
	for i, tag := range d.config.Tags {
		b, err := os.ReadFile(filepath.Join(os.Getenv("OPENDANGO_TEST_STORE"), url.PathEscape(tag)))
		if err != nil || (i > 0 && string(b) != id) {
			id = ""
			break
		}
		id = string(b)
	}
	if id == "" && !d.config.AllowMissing {
		return cty.NilVal, fmt.Errorf("no artifact with the tags %q", d.config.Tags)
	}
	return cty.ObjectVal(map[string]cty.Value{"id": cty.StringVal(id), "name": cty.StringVal("lookup from " + d.config.SamePoolAs)}), nil
}

// storeTags records the tags of an artifact in the store, when the test set
// one.
func storeTags(tags []string, id string) error {
	dir := os.Getenv("OPENDANGO_TEST_STORE")
	if dir == "" {
		return nil
	}
	for _, tag := range tags {
		if err := os.WriteFile(filepath.Join(dir, url.PathEscape(tag)), []byte(id), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// The "xva" post-processor writes "xva of <artifact id> compress=<c>" to
// its output, like the one of packer-plugin-xenorchestra exports the
// template.
type ppConfig struct {
	Output   string `mapstructure:"output"`
	Compress string `mapstructure:"compress"`
	Endpoint string `mapstructure:"endpoint"`
	Token    string `mapstructure:"token"`
	Insecure string `mapstructure:"insecure"`
}

type postProcessor struct{ config ppConfig }

func (p *postProcessor) ConfigSpec() hcldec.ObjectSpec { return hcldec.ObjectSpec{} }

func (p *postProcessor) Configure(raws ...any) error {
	return config.Decode(&p.config, &config.DecodeOpts{}, raws...)
}

func (p *postProcessor) PostProcess(ctx context.Context, ui packersdk.Ui, a packersdk.Artifact) (packersdk.Artifact, bool, bool, error) {
	if err := os.MkdirAll(filepath.Dir(p.config.Output), 0o755); err != nil {
		return nil, true, false, err
	}
	content := fmt.Sprintf("xva of %s compress=%s", a.Id(), p.config.Compress)
	if err := os.WriteFile(p.config.Output, []byte(content), 0o644); err != nil {
		return nil, true, false, err
	}
	ui.Say("exported " + p.config.Output)
	return &artifact{root: p.config.Output}, true, false, nil
}

func main() {
	set := plugin.NewSet()
	set.RegisterBuilder("local", new(Builder))
	set.RegisterDatasource("template", new(datasource))
	set.RegisterPostProcessor("xva", new(postProcessor))
	if err := set.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
