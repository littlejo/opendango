// Command packer-plugin-opendangotest is a Packer plugin for opendango's
// tests: its "local" builder (source opendangotest-local) provisions the
// machine running the tests, in the directory set by the "root" setting,
// through a communicator that runs /bin/sh locally. It drives opendango
// through the real plugin protocol, without any infrastructure.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

func (b *Builder) ConfigSpec() hcldec.ObjectSpec { return specOf(reflect.TypeOf(Config{})) }

// specOf describes the settings of a configuration struct by its
// mapstructure tags, like the code packer-sdc generates for real plugins.
func specOf(t reflect.Type) hcldec.ObjectSpec {
	spec := hcldec.ObjectSpec{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("mapstructure")
		if tag == ",squash" {
			for k, v := range specOf(f.Type) {
				spec[k] = v
			}
			continue
		}
		if tag == "" {
			continue
		}
		var ty cty.Type
		switch f.Type.Kind() {
		case reflect.String:
			ty = cty.String
		case reflect.Int, reflect.Int64:
			ty = cty.Number
		case reflect.Bool:
			ty = cty.Bool
		case reflect.Slice:
			ty = cty.List(cty.String)
		default:
			// cty's dynamic type does not go through the RPC (gob).
			ty = cty.String
		}
		spec[tag] = &hcldec.AttrSpec{Name: tag, Type: ty}
	}
	return spec
}

func (b *Builder) Prepare(raws ...any) ([]string, []string, error) {
	if err := config.Decode(&b.config, &config.DecodeOpts{}, raws...); err != nil {
		return nil, nil, err
	}
	// A machine opendango builds from a cached template only gets the
	// inherited settings: the tests give its root this way.
	if b.config.Root == "" {
		b.config.Root = os.Getenv("OPENDANGO_TEST_ROOT")
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
	data := map[string]any{"ConnType": "ssh", "Host": "127.0.0.1", "Port": c.SSHPort, "User": c.SSHUsername, "SSHPrivateKey": "PRIVATE KEY", "PackerHTTPAddr": "ERR_HTTP_ADDR_NOT_IMPLEMENTED_BY_BUILDER"}
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
	// The file of the artifact (Files), like a disk image.
	if err := os.WriteFile(filepath.Join(c.Root, "image.qcow2"), []byte("image of "+c.Root), 0o644); err != nil {
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
	cmd.Stdin, cmd.Stdout, cmd.Stderr = rc.Stdin, rc.Stdout, rc.Stderr
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
func (a *artifact) Files() []string   { return []string{filepath.Join(a.root, "image.qcow2")} }
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
		"id":    &hcldec.AttrSpec{Name: "id", Type: cty.String},
		"name":  &hcldec.AttrSpec{Name: "name", Type: cty.String},
		"tags":  &hcldec.AttrSpec{Name: "tags", Type: cty.List(cty.String)},
		"count": &hcldec.AttrSpec{Name: "count", Type: cty.Number},
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
	tags := cty.ListValEmpty(cty.String)
	if len(d.config.Tags) > 0 {
		var vals []cty.Value
		for _, t := range d.config.Tags {
			vals = append(vals, cty.StringVal(t))
		}
		tags = cty.ListVal(vals)
	}
	return cty.ObjectVal(map[string]cty.Value{
		"id":    cty.StringVal(id),
		"name":  cty.StringVal("lookup from " + d.config.SamePoolAs),
		"tags":  tags,
		"count": cty.NumberIntVal(int64(len(d.config.Tags))),
	}), nil
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

// The "inspect" post-processor writes what it gets to its output: the id,
// the builder id and the files of the artifact. Its artifact is the output
// file; keep=false asks to delete its input artifact.
type inspector struct {
	config struct {
		Output string `mapstructure:"output"`
		Note   string `mapstructure:"note"`
		Keep   string `mapstructure:"keep"`
	}
}

func (p *inspector) ConfigSpec() hcldec.ObjectSpec { return hcldec.ObjectSpec{} }

func (p *inspector) Configure(raws ...any) error {
	return config.Decode(&p.config, &config.DecodeOpts{}, raws...)
}

func (p *inspector) PostProcess(ctx context.Context, ui packersdk.Ui, a packersdk.Artifact) (packersdk.Artifact, bool, bool, error) {
	content := fmt.Sprintf("id=%s\nbuilder=%s\nfiles=%s\nnote=%s\n", a.Id(), a.BuilderId(), strings.Join(a.Files(), ","), p.config.Note)
	if err := os.WriteFile(p.config.Output, []byte(content), 0o644); err != nil {
		return nil, true, false, err
	}
	return fileArtifact(p.config.Output), p.config.Keep != "false", false, nil
}

// fileArtifact is a file; Destroy removes it.
type fileArtifact string

func (a fileArtifact) BuilderId() string { return "opendango.test.inspect" }
func (a fileArtifact) Files() []string   { return []string{string(a)} }
func (a fileArtifact) Id() string        { return "inspected:" + string(a) }
func (a fileArtifact) String() string    { return string(a) }
func (a fileArtifact) State(string) any  { return nil }
func (a fileArtifact) Destroy() error    { return os.Remove(string(a)) }

// The "shell" provisioner runs command through the communicator, uploads
// a file, downloads it back (alone and with its directory), and writes to
// report (on this machine) what it got: the generated data and the output.
type shellProvisioner struct {
	config struct {
		Command  string   `mapstructure:"command"`
		Report   string   `mapstructure:"report"`
		UploadTo string   `mapstructure:"upload_to"`
		Args     []string `mapstructure:"args"`
		Fail     bool     `mapstructure:"fail"`
	}
}

func (p *shellProvisioner) ConfigSpec() hcldec.ObjectSpec { return hcldec.ObjectSpec{} }

func (p *shellProvisioner) Prepare(raws ...any) error {
	if err := config.Decode(&p.config, &config.DecodeOpts{}, raws...); err != nil {
		return err
	}
	if p.config.Report == "" {
		return errors.New("report is required")
	}
	return nil
}

func (p *shellProvisioner) Provision(ctx context.Context, ui packersdk.Ui, comm packersdk.Communicator, data map[string]any) error {
	if p.config.Fail {
		return errors.New("failing as asked")
	}
	var out bytes.Buffer
	cmd := &packersdk.RemoteCmd{Command: p.config.Command, Stdout: &out, Stderr: &out}
	if err := cmd.RunWithUi(ctx, comm, ui); err != nil {
		return err
	}
	if cmd.ExitStatus() != 0 {
		return fmt.Errorf("%s: exit %d", p.config.Command, cmd.ExitStatus())
	}
	// stdin reaches the command (Ansible's piped transfers).
	var piped bytes.Buffer
	cat := &packersdk.RemoteCmd{Command: "cat", Stdin: strings.NewReader("through stdin"), Stdout: &piped}
	if err := cat.RunWithUi(ctx, comm, ui); err != nil {
		return err
	}
	report := fmt.Sprintf("stdin=%s\noutput=%s\nconn=%v user=%v port=%v host=%v key=%v\nargs=%s\n", piped.String(), strings.TrimSpace(out.String()), data["ConnType"], data["User"], data["Port"], data["Host"] != "", strings.Contains(fmt.Sprint(data["SSHPrivateKey"]), "PRIVATE KEY"), strings.Join(p.config.Args, "|"))
	if p.config.UploadTo != "" {
		if err := comm.Upload(p.config.UploadTo+"/sub/up.txt", strings.NewReader("uploaded"), nil); err != nil {
			return err
		}
		var back bytes.Buffer
		if err := comm.Download(p.config.UploadTo+"/sub/up.txt", &back); err != nil {
			return err
		}
		dir := p.config.Report + ".dir"
		if err := comm.DownloadDir(p.config.UploadTo+"/sub", dir, nil); err != nil {
			return err
		}
		inDir, _ := os.ReadFile(filepath.Join(dir, "up.txt"))
		report += "download=" + back.String() + " dir=" + string(inDir) + "\n"
	}
	return os.WriteFile(p.config.Report, []byte(report), 0o644)
}

func main() {
	set := plugin.NewSet()
	set.RegisterBuilder("local", new(Builder))
	set.RegisterDatasource("template", new(datasource))
	set.RegisterPostProcessor("xva", new(postProcessor))
	set.RegisterPostProcessor("inspect", new(inspector))
	set.RegisterProvisioner("shell", new(shellProvisioner))
	if err := set.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
