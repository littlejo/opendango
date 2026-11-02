// Package build runs a plan: it starts the VM, executes the instructions
// over the connection, stops the VM, produces the outputs and cleans up.
package build

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/littlejo/opendango/builders/xo"
	sshcomm "github.com/littlejo/opendango/internal/communicator/ssh"
	"github.com/littlejo/opendango/internal/instructions"
	"github.com/littlejo/opendango/internal/plan"
)

// Builder creates and drives the VM of one stage on a given infrastructure.
type Builder interface {
	// Start creates the VM and boots it with the given cloud-config
	// (empty when no connection is needed).
	Start(ctx context.Context, cloudConfig string) error
	// Address waits until the VM reports an IP address and returns it.
	Address(ctx context.Context) (string, error)
	// Stop waits up to grace for the VM to halt by itself, then shuts it
	// down, forcing it if needed.
	Stop(ctx context.Context, grace time.Duration) error
	// Output produces an artifact from the stopped VM. For xva, out.Path
	// is absolute. After a template output, the VM is a template.
	Output(ctx context.Context, out *instructions.Output) error
	// Destroy deletes what the builder created: the VM, or the template it
	// became. It is a no-op when nothing was created.
	Destroy(ctx context.Context) error
	// ID identifies the VM; once converted, it identifies the template, and
	// is what NewBuilder receives as parentTemplate for the child stages.
	ID() string
}

// Communicator runs commands in the VM and uploads files to it.
type Communicator interface {
	Run(ctx context.Context, command string, stdout, stderr io.Writer) error
	// Upload copies a local file or directory to dst, creating the parent
	// directories of dst.
	Upload(ctx context.Context, src, dst string) error
	Close() error
}

// DialFunc opens one connection to the VM.
type DialFunc func(ctx context.Context, host string, port int, user string, signer gossh.Signer) (Communicator, error)

// NewBuilderFunc returns the builder of a stage. parentTemplate is the ID of
// the parent stage's template, empty for FROM <builder>://<source>.
type NewBuilderFunc func(stage *plan.Stage, parentTemplate string) (Builder, error)

// Options configure a build.
type Options struct {
	// ContextDir is the directory COPY sources and OUTPUT paths are
	// relative to: the Dangofile's directory.
	ContextDir string
	// Target builds only this stage and its ancestors; every stage when
	// empty.
	Target string
	// KeepOnError leaves the VM and the intermediate templates in place
	// when the build fails.
	KeepOnError bool
	// Out receives the build log.
	Out io.Writer
	// NewBuilder creates the builder of each stage; NewBuilder when nil.
	NewBuilder NewBuilderFunc
	// Dial opens connections; SSH when nil.
	Dial DialFunc
	// RetryDelay is the pause between connection attempts; 5s when zero.
	RetryDelay time.Duration
}

// NewBuilder returns the builder of the stage's root FROM.
func NewBuilder(stage *plan.Stage, parentTemplate string, out io.Writer) (Builder, error) {
	root := stage.Root().From
	switch root.Builder {
	case "xo":
		rootCfg, err := xo.ParseConfig(root.Source, root.Flags)
		if err != nil {
			return nil, err
		}
		cfg := rootCfg
		if stage.Parent != nil {
			if cfg, err = xo.ParseStageConfig(stage.From.Flags); err != nil {
				return nil, err
			}
			// The parent template is in the root's pool.
			cfg.Template, cfg.Pool = parentTemplate, rootCfg.Pool
		}
		client, err := xo.NewClientFromEnv()
		if err != nil {
			return nil, err
		}
		name := stage.Name()
		return xo.NewBuilder(cfg, client, vmName(name), func(msg string) { say(out, name, "%s", msg) }), nil
	default:
		return nil, fmt.Errorf("unknown builder %q", root.Builder)
	}
}

// Run executes the plan: the selected stages, in order. A stage that other
// selected stages start from ends as a template: its OUTPUT --as=template,
// or a temporary one deleted at the end of the build.
func Run(ctx context.Context, p *plan.Plan, opts Options) (err error) {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.Dial == nil {
		opts.Dial = dialSSH
	}
	if opts.RetryDelay == 0 {
		opts.RetryDelay = 5 * time.Second
	}
	if opts.NewBuilder == nil {
		opts.NewBuilder = func(s *plan.Stage, parent string) (Builder, error) { return NewBuilder(s, parent, opts.Out) }
	}
	stages, err := p.Select(opts.Target)
	if err != nil {
		return err
	}
	selected := map[*plan.Stage]bool{}
	for _, s := range stages {
		selected[s] = true
	}

	done := map[*plan.Stage]*executor{}
	var temporary []*executor
	defer func() {
		if err != nil && opts.KeepOnError {
			for _, e := range temporary {
				e.say("Build failed, keeping intermediate template %s (--on-error=keep)", e.b.ID())
			}
			return
		}
		// Children first: a template cannot always be deleted while VMs
		// created from it exist.
		for i := len(temporary) - 1; i >= 0; i-- {
			e := temporary[i]
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
			e.say("Deleting intermediate template %s", e.b.ID())
			if derr := e.b.Destroy(cctx); derr != nil {
				err = errors.Join(err, fmt.Errorf("deleting intermediate template %s: %w", e.b.ID(), derr))
			}
			cancel()
		}
	}()

	for _, s := range stages {
		needed := false
		for _, c := range s.Children {
			needed = needed || selected[c]
		}
		e, err := runStage(ctx, p, s, done[s.Parent], needed, opts)
		if err != nil {
			return err
		}
		done[s] = e
		if needed && s.TemplateOutput() == nil {
			temporary = append(temporary, e)
		}
	}
	fmt.Fprintln(opts.Out, "==> Build finished")
	return nil
}

// runStage builds one stage. parent is the executor of the parent stage
// (nil for a root stage), whose final USER, WORKDIR and ENV are inherited.
// needed is true when a selected stage starts from this one: the VM then
// always ends as a template.
func runStage(ctx context.Context, p *plan.Plan, stage *plan.Stage, parent *executor, needed bool, opts Options) (_ *executor, err error) {
	parentTemplate := ""
	if parent != nil {
		parentTemplate = parent.b.ID()
	}
	b, err := opts.NewBuilder(stage, parentTemplate)
	if err != nil {
		return nil, err
	}
	e := &executor{
		file:       p.File,
		name:       stage.Name(),
		out:        opts.Out,
		b:          b,
		connect:    stage.Connect,
		dial:       opts.Dial,
		retryDelay: opts.RetryDelay,
		contextDir: opts.ContextDir,
	}
	if parent != nil {
		// Like a Docker image: USER, WORKDIR and ENV carry over, ARG does not.
		e.user, e.workdir = parent.user, parent.workdir
		e.env = append([]instructions.KeyValue{}, parent.env...)
	}

	cloudConfig := ""
	if e.connect != nil {
		authorizedKey, err := e.generateKey()
		if err != nil {
			return nil, err
		}
		if e.user == "" {
			e.user = e.connect.User
		}
		cloudConfig = CloudConfig(e.connect.User, authorizedKey)
	}

	converted := false
	defer func() {
		if converted {
			return
		}
		if err != nil && opts.KeepOnError {
			e.say("Build failed, keeping VM %s (--on-error=keep)", b.ID())
			return
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
		defer cancel()
		e.say("Destroying VM %s", b.ID())
		if derr := b.Destroy(cctx); derr != nil {
			err = errors.Join(err, fmt.Errorf("destroying VM %s: %w", b.ID(), derr))
		}
	}()

	e.say("Starting VM")
	if err := b.Start(ctx, cloudConfig); err != nil {
		return nil, fmt.Errorf("starting VM: %w", err)
	}

	if e.connect != nil {
		if err := e.open(ctx); err != nil {
			return nil, err
		}
		defer func() {
			if e.comm != nil {
				e.comm.Close()
			}
		}()
	}

	for _, step := range stage.Steps {
		if err := e.step(ctx, step); err != nil {
			return nil, &plan.Error{File: p.File, Line: step.Line(), Err: err}
		}
	}

	grace := time.Duration(0)
	if e.comm != nil {
		if err := e.finish(ctx); err != nil {
			return nil, err
		}
		grace = 5 * time.Minute
	}
	e.say("Stopping VM")
	if err := b.Stop(ctx, grace); err != nil {
		return nil, fmt.Errorf("stopping VM: %w", err)
	}

	// Exports first: the conversion to a template is the last thing done
	// to the VM.
	for _, out := range stage.Outputs {
		if out.Kind != instructions.OutputXVA {
			continue
		}
		o := *out
		if !filepath.IsAbs(o.Path) {
			o.Path = filepath.Join(opts.ContextDir, o.Path)
		}
		e.say("Exporting XVA to %s", o.Path)
		if err := b.Output(ctx, &o); err != nil {
			return nil, &plan.Error{File: p.File, Line: out.Line(), Err: err}
		}
	}
	if out := stage.TemplateOutput(); out != nil {
		e.say("Converting VM %s to a template", b.ID())
		if err := b.Output(ctx, out); err != nil {
			return nil, &plan.Error{File: p.File, Line: out.Line(), Err: err}
		}
		converted = true
	} else if needed {
		name := fmt.Sprintf("opendango-tmp-%s-%d", stage.Name(), time.Now().Unix())
		e.say("Converting VM %s to the intermediate template %s", b.ID(), name)
		if err := b.Output(ctx, &instructions.Output{Kind: instructions.OutputTemplate, Name: name}); err != nil {
			return nil, fmt.Errorf("converting to an intermediate template: %w", err)
		}
		converted = true
	}
	e.say("Stage finished")
	return e, nil
}

type executor struct {
	file       string
	name       string
	out        io.Writer
	b          Builder
	comm       Communicator
	connect    *instructions.Connect
	dial       DialFunc
	retryDelay time.Duration
	contextDir string

	signer     gossh.Signer
	keyComment string

	// State changed by the instructions.
	user    string
	workdir string
	args    []instructions.KeyValue
	env     []instructions.KeyValue
}

func (e *executor) say(format string, a ...any) { say(e.out, e.name, format, a...) }

func say(out io.Writer, name, format string, a ...any) {
	fmt.Fprintf(out, "==> %s: %s\n", name, fmt.Sprintf(format, a...))
}

// generateKey creates the ephemeral SSH key and returns its authorized_keys
// line. The comment identifies it so that finish can remove it.
func (e *executor) generateKey() (string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	if e.signer, err = gossh.NewSignerFromKey(priv); err != nil {
		return "", err
	}
	sshPub, err := gossh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	id := make([]byte, 6)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	e.keyComment = "opendango-" + hex.EncodeToString(id)
	return strings.TrimSpace(string(gossh.MarshalAuthorizedKey(sshPub))) + " " + e.keyComment, nil
}

// open connects to the VM, retrying until the CONNECT timeout.
func (e *executor) open(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, e.connect.Timeout)
	defer cancel()
	e.say("Waiting for the VM IP address")
	var lastErr error
	for {
		host, err := e.b.Address(ctx)
		if err == nil {
			e.say("Connecting to %s@%s:%d", e.connect.User, host, e.connect.Port)
			e.comm, err = e.dial(ctx, host, e.connect.Port, e.connect.User, e.signer)
			if err == nil {
				return nil
			}
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("connecting to the VM: timeout after %s: %w", e.connect.Timeout, lastErr)
		case <-time.After(e.retryDelay):
		}
	}
}

func (e *executor) step(ctx context.Context, step instructions.Instruction) error {
	switch s := step.(type) {
	case *instructions.Arg:
		if s.Set {
			e.args = setVar(e.args, s.Name, s.Value)
		}
		return nil
	case *instructions.Env:
		for _, kv := range s.Vars {
			e.env = setVar(e.env, kv.Key, kv.Value)
		}
		if s.Persist {
			return e.persistEnv(ctx, s)
		}
		return nil
	case *instructions.User:
		e.user = s.Name
		return nil
	case *instructions.Workdir:
		dir := s.Path
		if !path.IsAbs(dir) && e.workdir != "" {
			dir = path.Join(e.workdir, dir)
		}
		e.say("WORKDIR %s", dir)
		if err := e.run(ctx, e.wrap("mkdir -p "+instructions.ShellQuote(dir))); err != nil {
			return fmt.Errorf("WORKDIR: %w", err)
		}
		e.workdir = dir
		return nil
	case *instructions.Run:
		e.say("RUN %s", firstLine(s.Script))
		if err := e.run(ctx, e.wrap(s.Script)); err != nil {
			return fmt.Errorf("RUN: %w", err)
		}
		return nil
	case *instructions.Copy:
		e.say("COPY %s %s", strings.Join(s.Sources, " "), s.Dest)
		if err := e.copy(ctx, s); err != nil {
			return fmt.Errorf("COPY: %w", err)
		}
		return nil
	case *instructions.Reboot:
		e.say("REBOOT")
		if err := e.reboot(ctx); err != nil {
			return fmt.Errorf("REBOOT: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unexpected instruction %T", step)
	}
}

// wrap builds the command that runs script as the current USER, in the
// current WORKDIR, with the ARG and ENV values exported.
func (e *executor) wrap(script string) string {
	var b strings.Builder
	// ENV after ARG: on a name clash, ENV wins, like in Docker.
	for _, kv := range append(append([]instructions.KeyValue{}, e.args...), e.env...) {
		fmt.Fprintf(&b, "export %s=%s\n", kv.Key, instructions.ShellQuote(kv.Value))
	}
	if e.workdir != "" {
		fmt.Fprintf(&b, "cd %s || exit 1\n", instructions.ShellQuote(e.workdir))
	}
	b.WriteString(script)
	cmd := "/bin/sh -c " + instructions.ShellQuote(b.String())
	if e.user == e.connect.User {
		return cmd
	}
	return "sudo -n -H -u " + instructions.ShellQuote(e.user) + " -- " + cmd
}

// asRoot runs a command as root, whatever the current USER.
func (e *executor) asRoot(cmd string) string {
	if e.connect.User == "root" {
		return cmd
	}
	return "sudo -n -- " + cmd
}

func (e *executor) run(ctx context.Context, cmd string) error {
	w := newPrefixWriter(e.out, "    "+e.name+": ")
	defer w.Flush()
	return e.comm.Run(ctx, cmd, w, w)
}

// output runs cmd as the connection user and returns its trimmed stdout.
func (e *executor) output(ctx context.Context, cmd string) (string, error) {
	var stdout strings.Builder
	errw := newPrefixWriter(e.out, "    "+e.name+": ")
	defer errw.Flush()
	if err := e.comm.Run(ctx, cmd, &stdout, errw); err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (e *executor) persistEnv(ctx context.Context, s *instructions.Env) error {
	var script strings.Builder
	for _, kv := range s.Vars {
		e.say("ENV --persist %s", kv.Key)
		fmt.Fprintf(&script, "sed -i '/^%s=/d' /etc/environment && printf '%%s=\"%%s\"\\n' %s %s >> /etc/environment\n",
			kv.Key, kv.Key, instructions.ShellQuote(kv.Value))
	}
	if err := e.run(ctx, e.asRoot("/bin/sh -c "+instructions.ShellQuote("set -e\n"+script.String()))); err != nil {
		return fmt.Errorf("ENV --persist: %w", err)
	}
	return nil
}

// copy uploads the sources to a temporary directory as the connection user,
// then copies them into place as the current USER.
func (e *executor) copy(ctx context.Context, c *instructions.Copy) error {
	var srcs []string
	for _, pattern := range c.Sources {
		matches, err := e.resolveSource(pattern)
		if err != nil {
			return err
		}
		srcs = append(srcs, matches...)
	}

	tmp, err := e.output(ctx, "mktemp -d /tmp/opendango.XXXXXX")
	if err != nil {
		return fmt.Errorf("creating a temporary directory: %w", err)
	}
	defer func() {
		_ = e.comm.Run(context.WithoutCancel(ctx), "rm -rf "+instructions.ShellQuote(tmp), io.Discard, io.Discard)
	}()
	// The current USER may not be the connection user: let it read the files.
	if err := e.run(ctx, "chmod 755 "+instructions.ShellQuote(tmp)); err != nil {
		return err
	}

	toDir := len(srcs) > 1 || strings.HasSuffix(c.Dest, "/")
	var script strings.Builder
	script.WriteString("set -e\n")
	for i, src := range srcs {
		info, err := os.Stat(src)
		if err != nil {
			return err
		}
		remote := path.Join(tmp, fmt.Sprint(i), filepath.Base(src))
		if err := e.comm.Upload(ctx, src, remote); err != nil {
			return fmt.Errorf("uploading %s: %w", src, err)
		}

		var targets []string
		switch {
		case info.IsDir():
			// Like Docker, a directory's contents are copied, not the
			// directory itself.
			fmt.Fprintf(&script, "mkdir -p %s\ncp -R %s/. %s/\n", q(c.Dest), q(remote), q(c.Dest))
			entries, err := os.ReadDir(src)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				targets = append(targets, path.Join(c.Dest, entry.Name()))
			}
		case toDir:
			target := path.Join(c.Dest, filepath.Base(src))
			fmt.Fprintf(&script, "mkdir -p %s\ncp %s %s\n", q(c.Dest), q(remote), q(target))
			targets = append(targets, target)
		default:
			fmt.Fprintf(&script, "mkdir -p %s\ncp %s %s\n", q(path.Dir(c.Dest)), q(remote), q(c.Dest))
			targets = append(targets, c.Dest)
		}
		for _, t := range targets {
			if c.Chown != "" {
				fmt.Fprintf(&script, "chown -R %s %s\n", q(c.Chown), q(t))
			}
			if c.Chmod != "" {
				fmt.Fprintf(&script, "chmod -R %s %s\n", c.Chmod, q(t))
			}
		}
	}
	return e.run(ctx, e.wrap(script.String()))
}

// resolveSource expands a COPY source relative to the build context and
// refuses anything outside of it.
func (e *executor) resolveSource(pattern string) ([]string, error) {
	if filepath.IsAbs(pattern) {
		return nil, fmt.Errorf("%s: sources must be relative to the Dangofile directory", pattern)
	}
	matches, err := filepath.Glob(filepath.Join(e.contextDir, pattern))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", pattern, err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("%s: no such file or directory", pattern)
	}
	for _, m := range matches {
		rel, err := filepath.Rel(e.contextDir, m)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%s: sources must stay inside the Dangofile directory", pattern)
		}
	}
	return matches, nil
}

// reboot reboots the VM and waits until it is reachable with a new boot ID.
func (e *executor) reboot(ctx context.Context) error {
	const bootIDCmd = "cat /proc/sys/kernel/random/boot_id"
	before, err := e.output(ctx, bootIDCmd)
	if err != nil {
		return fmt.Errorf("reading the boot ID: %w", err)
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	// The connection drops while the command runs: its error is expected.
	_ = e.comm.Run(rctx, e.asRoot("reboot"), io.Discard, io.Discard)
	cancel()
	e.comm.Close()
	e.comm = nil

	deadline := time.Now().Add(e.connect.Timeout)
	for {
		if err := e.open(ctx); err != nil {
			return err
		}
		after, err := e.output(ctx, bootIDCmd)
		if err == nil && after != before {
			return nil
		}
		e.comm.Close()
		e.comm = nil
		if time.Now().After(deadline) {
			return fmt.Errorf("the VM did not reboot within %s", e.connect.Timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(e.retryDelay):
		}
	}
}

// finish removes the ephemeral key from the image and shuts the VM down.
func (e *executor) finish(ctx context.Context) error {
	e.say("Removing the temporary SSH key")
	cmd := "if [ -f ~/.ssh/authorized_keys ]; then sed -i '/ " + e.keyComment + "$/d' ~/.ssh/authorized_keys; fi"
	if err := e.run(ctx, cmd); err != nil {
		return fmt.Errorf("removing the temporary SSH key: %w", err)
	}
	e.say("Shutting down")
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// The connection drops while the command runs: its error is expected.
	_ = e.comm.Run(sctx, e.asRoot("shutdown -h now"), io.Discard, io.Discard)
	e.comm.Close()
	e.comm = nil
	return nil
}

func setVar(vars []instructions.KeyValue, key, value string) []instructions.KeyValue {
	for i := range vars {
		if vars[i].Key == key {
			vars[i].Value = value
			return vars
		}
	}
	return append(vars, instructions.KeyValue{Key: key, Value: value})
}

func q(s string) string { return instructions.ShellQuote(s) }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " ..."
	}
	return s
}

func vmName(stage string) string {
	return fmt.Sprintf("opendango-%s-%d", stage, time.Now().Unix())
}

func dialSSH(ctx context.Context, host string, port int, user string, signer gossh.Signer) (Communicator, error) {
	return sshcomm.Dial(ctx, sshcomm.Config{Host: host, Port: port, User: user, Signer: signer})
}

// CloudConfig returns the cloud-config that authorizes the ephemeral key
// for user. The template's default user is kept.
func CloudConfig(user, authorizedKey string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	if user == "root" {
		b.WriteString("disable_root: false\n")
	}
	fmt.Fprintf(&b, "users:\n  - default\n  - name: %q\n    ssh_authorized_keys:\n      - %q\n", user, authorizedKey)
	return b.String()
}
