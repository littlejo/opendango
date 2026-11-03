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
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/hashicorp/packer-plugin-sdk/common"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/plugin"
	"github.com/hashicorp/packer-plugin-sdk/template/config"
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
	if c.HTTPDir != "" {
		ui.Say(fmt.Sprintf("http: dir=%s ports=%d-%d", c.HTTPDir, c.HTTPPortMin, c.HTTPPortMax))
	}
	if err := hook.Run(ctx, packersdk.HookProvision, ui, &comm{root: c.Root}, nil); err != nil {
		if c.PackerOnError == "ask" {
			answer, _ := ui.Ask("[c] Clean up and exit, [a] abort without cleanup, or [r] retry step?")
			ui.Say("cleanup answer: " + answer)
		}
		return nil, err
	}
	if c.Fail {
		return nil, errors.New("failing as asked")
	}
	return &artifact{root: c.Root}, nil
}

// comm runs commands with /bin/sh in root.
type comm struct{ root string }

func (c *comm) Start(ctx context.Context, rc *packersdk.RemoteCmd) error {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", rc.Command)
	cmd.Dir = c.root
	cmd.Env = append(os.Environ(), "HOME="+c.root)
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
func (a *artifact) Destroy() error    { return nil }

func main() {
	set := plugin.NewSet()
	set.RegisterBuilder("local", new(Builder))
	if err := set.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
