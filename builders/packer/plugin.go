package packer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"

	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	packerrpc "github.com/hashicorp/packer-plugin-sdk/rpc"
)

// The handshake of packer-plugin-sdk/plugin: the cookie lets the plugin
// know it is driven, and apiMajor is the RPC protocol it must speak.
const (
	cookieKey   = "PACKER_PLUGIN_MAGIC_COOKIE"
	cookieValue = "d602bf8f470bc67ca7faa0386276bbdd4330efaf76d1a219cb4d6991ca9872b2"
	apiMajor    = "5"
)

// Build is one run of a plugin builder.
type Build struct {
	Plugin  *Plugin
	Builder string
	// Dir is the working directory of the plugin: relative paths in its
	// configuration are relative to it (the Dangofile directory).
	Dir string
	// Configs are passed to the builder's Prepare, in order.
	Configs []map[string]any
	// Ui receives the plugin messages and answers its questions.
	Ui packersdk.Ui
	// Provision runs the instructions with the plugin's communicator and
	// what the plugin told about the machine.
	Provision func(ctx context.Context, comm *Comm, data HookData) error
	// DestroyArtifact deletes the artifact once built (the stage has no
	// OUTPUT --as=template): only its files, such as an XVA, stay.
	DestroyArtifact bool
	// KeepPlugin keeps the plugin running after Run, for a later
	// Artifact.Destroy (an intermediate template, deleted at the end of the
	// build): the artifact can only be destroyed through the plugin that
	// made it. Artifact.Close stops it.
	KeepPlugin bool
}

// Artifact is what a build produced.
type Artifact struct {
	// ID is the plugin's id of the artifact (xenorchestra: the template
	// UUID).
	ID string
	// Desc describes it, for the log.
	Desc  string
	Files []string
	// BuilderID is the builder's id of the artifact type, which some
	// post-processors read (vagrant picks its provider by it).
	BuilderID string

	art packersdk.Artifact
	// p is the plugin, kept with KeepPlugin.
	p      *process
	plugin string
}

// Destroy deletes the artifact through the plugin, which must still run
// (KeepPlugin), then stops the plugin.
func (a *Artifact) Destroy() error {
	if a.p == nil {
		return fmt.Errorf("plugin: %s: the plugin of the artifact %s is not running anymore", a.plugin, a.ID)
	}
	err := a.art.Destroy()
	if err != nil {
		err = fmt.Errorf("plugin: %s: deleting the artifact %s: %w%s", a.plugin, a.ID, err, a.p.logTail())
	}
	a.Close()
	return err
}

// Close stops the plugin kept with KeepPlugin; Destroy is not possible
// afterwards.
func (a *Artifact) Close() {
	if a.p != nil {
		a.p.stop()
		a.p = nil
	}
}

// Run starts the plugin, prepares and runs the builder, and returns its
// artifact (nil when the builder produced none). The error of Provision is
// returned as is.
func (b *Build) Run(ctx context.Context) (_ *Artifact, err error) {
	p, err := start(b.Plugin, "builder", b.Builder, b.Dir)
	if err != nil {
		return nil, err
	}
	kept := false
	defer func() {
		if !kept {
			p.stop()
		}
	}()

	builder := p.client.Builder()
	configs := make([]any, len(b.Configs))
	for i, c := range b.Configs {
		configs[i] = c
	}
	_, warnings, err := builder.Prepare(configs...)
	for _, w := range warnings {
		b.Ui.Say("Warning: " + w)
	}
	if err != nil {
		return nil, fmt.Errorf("plugin: %s: invalid configuration: %w%s", b.Plugin.Name, err, p.logTail())
	}

	h := &hook{provision: b.Provision}
	art, err := builder.Run(ctx, b.Ui, h)
	if h.err != nil {
		return nil, h.err
	}
	if err != nil {
		return nil, fmt.Errorf("plugin: %s: %w%s", b.Plugin.Name, err, p.logTail())
	}
	if art == nil {
		return nil, nil
	}
	a := &Artifact{ID: art.Id(), Desc: art.String(), Files: art.Files(), BuilderID: art.BuilderId(), art: art, plugin: b.Plugin.Name}
	if b.DestroyArtifact {
		b.Ui.Say("Deleting the artifact: no OUTPUT --as=template")
		if err := art.Destroy(); err != nil {
			return a, fmt.Errorf("plugin: %s: deleting the artifact %s: %w%s", b.Plugin.Name, a.ID, err, p.logTail())
		}
	}
	if b.KeepPlugin {
		a.p, kept = p, true
	}
	return a, nil
}

// process is a running plugin.
type process struct {
	cmd    *exec.Cmd
	client *packerrpc.Client
	log    *tail
}

// start runs `<plugin> start builder <name>` and connects to the address
// it prints: <api major>|<api minor>|<network>|<address>.
func start(pl *Plugin, kind, name, dir string) (*process, error) {
	cmd := exec.Command(pl.Path, "start", kind, name)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), cookieKey+"="+cookieValue,
		// Used on Windows only, where the plugin listens on TCP.
		"PACKER_PLUGIN_MIN_PORT=10000", "PACKER_PLUGIN_MAX_PORT=25000")
	log := &tail{}
	cmd.Stderr = log
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("plugin: starting %s: %w", pl.Path, err)
	}
	p := &process{cmd: cmd, log: log}

	line := make(chan string, 1)
	go func() {
		r := bufio.NewReader(stdout)
		l, _ := r.ReadString('\n')
		line <- l
		// The plugin may write more; keep its stdout drained.
		io.Copy(io.Discard, r)
	}()
	var handshake string
	select {
	case handshake = <-line:
	case <-time.After(time.Minute):
		p.stop()
		return nil, fmt.Errorf("plugin: %s did not start within a minute%s", pl.Path, log.String())
	}
	parts := strings.Split(strings.TrimSpace(handshake), "|")
	if len(parts) != 4 || parts[0] != apiMajor {
		p.stop()
		return nil, fmt.Errorf("plugin: %s: unexpected handshake %q%s", pl.Path, handshake, log.String())
	}
	conn, err := net.Dial(parts[2], parts[3])
	if err != nil {
		p.stop()
		return nil, fmt.Errorf("plugin: connecting to %s: %w", pl.Path, err)
	}
	if p.client, err = packerrpc.NewClient(conn); err != nil {
		conn.Close()
		p.stop()
		return nil, fmt.Errorf("plugin: connecting to %s: %w", pl.Path, err)
	}
	return p, nil
}

// stop closes the connection, which ends the plugin, and kills it if it
// is still there after a while.
func (p *process) stop() {
	if p.client != nil {
		p.client.Close()
	}
	done := make(chan struct{})
	go func() {
		p.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		p.cmd.Process.Kill()
		<-done
	}
}

func (p *process) logTail() string { return p.log.String() }

// tail keeps the last lines the plugin wrote to stderr (its log), shown
// when it fails.
type tail struct {
	mu    sync.Mutex
	lines []string
	buf   string
}

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf += string(b)
	for {
		i := strings.IndexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		t.lines = append(t.lines, t.buf[:i])
		t.buf = t.buf[i+1:]
		if len(t.lines) > 20 {
			t.lines = t.lines[1:]
		}
	}
	return len(b), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.lines) == 0 {
		return ""
	}
	return "\nlast lines of the plugin log:\n  " + strings.Join(t.lines, "\n  ")
}

// HookData is what the plugin passes to the provision hook (the generated
// data of Packer), as far as opendango uses it. A field is empty when the
// plugin does not set it.
type HookData struct {
	// HTTPAddr is the address of the plugin's HTTP server (ip:port), which
	// the machine reaches (PackerHTTPAddr).
	HTTPAddr string
	// Host, Port and User are those of the SSH connection.
	Host string
	Port int
	User string
	// SSHPrivateKey is the private key of the connection (PEM), the
	// temporary one or the one of ssh_private_key_file.
	SSHPrivateKey string
	// Raw is all the data, for the provisioners (PROVISION).
	Raw map[string]any
}

// hookData reads the map the plugin passes. The SDK puts placeholders
// starting with ERR_ for what the builder does not implement.
func hookData(raw any) HookData {
	// The RPC (msgpack) turns map[string]any into map[any]any.
	m, _ := raw.(map[string]any)
	if mm, ok := raw.(map[any]any); ok {
		m = map[string]any{}
		for k, v := range mm {
			if ks, ok := k.(string); ok {
				m[ks] = v
			}
		}
	}
	str := func(k string) string {
		s, _ := m[k].(string)
		if strings.HasPrefix(s, "ERR_") {
			return ""
		}
		return s
	}
	d := HookData{HTTPAddr: str("PackerHTTPAddr"), Host: str("Host"), User: str("User"), SSHPrivateKey: str("SSHPrivateKey"), Raw: m}
	switch p := m["Port"].(type) {
	case int:
		d.Port = p
	case int64:
		d.Port = int(p)
	case uint64:
		d.Port = int(p)
	case float64:
		d.Port = int(p)
	}
	return d
}

// hook runs Provision on the provision hook and ignores the others.
type hook struct {
	provision func(ctx context.Context, comm *Comm, data HookData) error
	err       error
}

func (h *hook) Run(ctx context.Context, name string, _ packersdk.Ui, comm packersdk.Communicator, data any) error {
	if name != packersdk.HookProvision {
		return nil
	}
	if comm == nil {
		h.err = errors.New("plugin: no communicator: CONNECT is required to run instructions")
		return h.err
	}
	h.err = h.provision(ctx, &Comm{c: comm}, hookData(data))
	return h.err
}

// Comm runs commands and uploads files through the plugin's communicator.
type Comm struct {
	c packersdk.Communicator
}

// ExitError is a command that exited with a non-zero status.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit code %d", e.Code) }

// Run runs command and waits for it.
func (c *Comm) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	rc := &packersdk.RemoteCmd{Command: command, Stdout: stdout, Stderr: stderr}
	if err := c.c.Start(ctx, rc); err != nil {
		return err
	}
	status := make(chan int, 1)
	go func() { status <- rc.Wait() }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case code := <-status:
		switch code {
		case 0:
			return nil
		case packersdk.CmdDisconnect:
			return errors.New("the connection was lost")
		default:
			return &ExitError{Code: code}
		}
	}
}

// Upload copies a local file, or the contents of a local directory, to dst,
// creating the parent directories of dst.
func (c *Comm) Upload(ctx context.Context, src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := c.Run(ctx, "mkdir -p "+quote(dst), io.Discard, io.Discard); err != nil {
			return fmt.Errorf("creating %s: %w", dst, err)
		}
		// The trailing slash copies the contents, like rsync.
		return c.c.UploadDir(dst, strings.TrimSuffix(src, "/")+"/", nil)
	}
	if err := c.Run(ctx, "mkdir -p "+quote(path.Dir(dst)), io.Discard, io.Discard); err != nil {
		return fmt.Errorf("creating %s: %w", path.Dir(dst), err)
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	return c.c.Upload(dst, f, &info)
}

// Close does nothing: the plugin owns the connection.
func (c *Comm) Close() error { return nil }

// Raw returns the plugin's communicator, for a provisioner (PROVISION).
func (c *Comm) Raw() packersdk.Communicator { return c.c }

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
