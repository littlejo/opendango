package build

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	gossh "golang.org/x/crypto/ssh"

	"github.com/littlejo/opendango/builders/packer"
	sshcomm "github.com/littlejo/opendango/internal/communicator/ssh"
	"github.com/littlejo/opendango/internal/instructions"
)

// FindProvisioner looks for the plugin of a provisioner type (ansible,
// ansible-local) like FindPlugin, and checks that it has it.
func FindProvisioner(ctx context.Context, source string) (*packer.Plugin, string, error) {
	return findComponent(ctx, nil, "provisioner", source)
}

// provision runs PROVISION: a plugin's provisioner with the connection of
// the stage, like Packer runs its provisioners (as the connection's user;
// USER, ENV and WORKDIR do not apply).
func (e *executor) provision(ctx context.Context, s *instructions.Provision) error {
	cfg, err := packer.ParseConfig(s.Source, s.Settings)
	if err != nil {
		return err
	}
	pl, name, err := e.findProvisioner(ctx, cfg.Component)
	if err != nil {
		return err
	}
	// A plugin:// stage passes its plugin's communicator as is; the native
	// connection goes through an adapter.
	var comm packersdk.Communicator
	if pc, ok := e.comm.(*packer.Comm); ok {
		comm = pc.Raw()
	} else {
		comm = &commAdapter{ctx: ctx, c: e.comm}
	}
	data, err := e.connectionData()
	if err != nil {
		return err
	}
	core := map[string]any{
		"packer_build_name":          e.name,
		"packer_builder_type":        e.builderType,
		"packer_core_version":        "opendango",
		"packer_debug":               false,
		"packer_force":               false,
		"packer_on_error":            "cleanup",
		"packer_user_variables":      map[string]string{},
		"packer_sensitive_variables": cfg.Secrets,
	}
	ui := &packer.Ui{Print: func(line string) { e.say("%s", line) }, Secrets: cfg.Secrets}
	return packer.Provision(ctx, pl, name, e.contextDir, ui, comm, data, cfg.Settings, core)
}

// connectionData returns what a provisioner knows of the connection
// (Packer's generated data): the plugin's in a plugin:// stage, otherwise
// the native connection with its ephemeral key.
func (e *executor) connectionData() (map[string]any, error) {
	if e.pluginData != nil {
		return e.pluginData, nil
	}
	data := map[string]any{
		"ConnType":          "ssh",
		"Host":              e.host,
		"Port":              e.connect.Port,
		"User":              e.connect.User,
		"Password":          "",
		"SSHPrivateKeyFile": "",
		"SSHAgentAuth":      false,
		"ID":                "",
		"PackerHTTPAddr":    "ERR_HTTP_ADDR_NOT_IMPLEMENTED_BY_BUILDER",
	}
	if e.b != nil {
		data["ID"] = e.b.ID()
	}
	if e.serveAddr != "" {
		data["PackerHTTPAddr"] = e.serveAddr
	}
	if e.key != nil {
		block, err := gossh.MarshalPrivateKey(e.key, e.keyComment)
		if err != nil {
			return nil, err
		}
		data["SSHPrivateKey"] = string(pem.EncodeToMemory(block))
	}
	return data, nil
}

// commAdapter is the native connection seen as a Packer communicator, for
// the provisioners.
type commAdapter struct {
	ctx context.Context
	c   Communicator
}

func (a *commAdapter) Start(ctx context.Context, rc *packersdk.RemoteCmd) error {
	stdout, stderr := rc.Stdout, rc.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if rc.Stdin != nil {
		in, ok := a.c.(inputRunner)
		if !ok {
			return errors.New("opendango: this connection cannot pass stdin to a command")
		}
		go func() {
			rc.SetExited(exitCode(in.RunInput(ctx, rc.Command, rc.Stdin, stdout, stderr)))
		}()
		return nil
	}
	go func() {
		rc.SetExited(exitCode(a.c.Run(ctx, rc.Command, stdout, stderr)))
	}()
	return nil
}

// inputRunner is a connection that passes stdin to a command (the native
// SSH one).
type inputRunner interface {
	RunInput(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error
}

// exitCode is the exit status of a command, or the SDK's disconnection
// code when the connection failed.
func exitCode(err error) int {
	var sshErr *sshcomm.ExitError
	var pluginErr *packer.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &sshErr):
		return sshErr.Code
	case errors.As(err, &pluginErr):
		return pluginErr.Code
	default:
		return packersdk.CmdDisconnect
	}
}

func (a *commAdapter) Upload(dst string, r io.Reader, fi *os.FileInfo) error {
	f, err := os.CreateTemp("", "opendango-upload-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && fi != nil {
		err = os.Chmod(f.Name(), (*fi).Mode().Perm())
	}
	if err != nil {
		return err
	}
	return a.c.Upload(a.ctx, f.Name(), dst)
}

// UploadDir copies src into dst: its contents when src ends with /, the
// directory itself otherwise, like Packer's communicators. exclude is not
// supported.
func (a *commAdapter) UploadDir(dst, src string, exclude []string) error {
	if len(exclude) > 0 {
		return errors.New("opendango: UploadDir with exclude is not supported")
	}
	if !strings.HasSuffix(src, "/") {
		dst = path.Join(dst, filepath.Base(src))
	}
	return a.c.Upload(a.ctx, src, dst)
}

func (a *commAdapter) Download(src string, w io.Writer) error {
	var stderr strings.Builder
	if err := a.c.Run(a.ctx, "cat -- "+instructions.ShellQuote(src), w, &stderr); err != nil {
		return fmt.Errorf("downloading %s: %w: %s", src, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// DownloadDir copies the contents of src (in the machine) into dst, with
// tar. exclude is not supported.
func (a *commAdapter) DownloadDir(src, dst string, exclude []string) error {
	if len(exclude) > 0 {
		return errors.New("opendango: DownloadDir with exclude is not supported")
	}
	pr, pw := io.Pipe()
	extracted := make(chan error, 1)
	go func() {
		err := untar(pr, dst)
		io.Copy(io.Discard, pr)
		extracted <- err
	}()
	var stderr strings.Builder
	err := a.c.Run(a.ctx, "tar -C "+instructions.ShellQuote(src)+" -cf - .", pw, &stderr)
	pw.CloseWithError(err)
	if xerr := <-extracted; err == nil {
		err = xerr
	}
	if err != nil {
		return fmt.Errorf("downloading %s: %w: %s", src, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
