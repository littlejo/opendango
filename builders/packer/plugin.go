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
	// Provision runs the instructions with the plugin's communicator.
	Provision func(ctx context.Context, comm *Comm) error
}

// Run starts the plugin, prepares and runs the builder, and returns a
// description of the artifact. The error of Provision is returned as is.
func (b *Build) Run(ctx context.Context) (string, error) {
	p, err := start(b.Plugin, b.Builder, b.Dir)
	if err != nil {
		return "", err
	}
	defer p.stop()

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
		return "", fmt.Errorf("packer: %s: invalid configuration: %w%s", b.Plugin.Name, err, p.logTail())
	}

	h := &hook{provision: b.Provision}
	artifact, err := builder.Run(ctx, b.Ui, h)
	if h.err != nil {
		return "", h.err
	}
	if err != nil {
		return "", fmt.Errorf("packer: %s: %w%s", b.Plugin.Name, err, p.logTail())
	}
	if artifact == nil {
		return "no artifact", nil
	}
	return artifact.String(), nil
}

// process is a running plugin.
type process struct {
	cmd    *exec.Cmd
	client *packerrpc.Client
	log    *tail
}

// start runs `<plugin> start builder <name>` and connects to the address
// it prints: <api major>|<api minor>|<network>|<address>.
func start(pl *Plugin, builder, dir string) (*process, error) {
	cmd := exec.Command(pl.Path, "start", "builder", builder)
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
		return nil, fmt.Errorf("packer: starting %s: %w", pl.Path, err)
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
		return nil, fmt.Errorf("packer: %s did not start within a minute%s", pl.Path, log.String())
	}
	parts := strings.Split(strings.TrimSpace(handshake), "|")
	if len(parts) != 4 || parts[0] != apiMajor {
		p.stop()
		return nil, fmt.Errorf("packer: %s: unexpected handshake %q%s", pl.Path, handshake, log.String())
	}
	conn, err := net.Dial(parts[2], parts[3])
	if err != nil {
		p.stop()
		return nil, fmt.Errorf("packer: connecting to %s: %w", pl.Path, err)
	}
	if p.client, err = packerrpc.NewClient(conn); err != nil {
		conn.Close()
		p.stop()
		return nil, fmt.Errorf("packer: connecting to %s: %w", pl.Path, err)
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

// hook runs Provision on the provision hook and ignores the others.
type hook struct {
	provision func(ctx context.Context, comm *Comm) error
	err       error
}

func (h *hook) Run(ctx context.Context, name string, _ packersdk.Ui, comm packersdk.Communicator, _ any) error {
	if name != packersdk.HookProvision {
		return nil
	}
	if comm == nil {
		h.err = errors.New("packer: the plugin gave no communicator: CONNECT is required to run instructions")
		return h.err
	}
	h.err = h.provision(ctx, &Comm{c: comm})
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

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
