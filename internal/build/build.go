// Package build runs a plan: it starts the VM, executes the instructions
// over the connection, stops the VM, produces the outputs and cleans up.
package build

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
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
	// FindCache looks for a template tagged with the cache key, where the
	// VM would be created. When found, the builder stands for it: ID
	// returns it, Output exports it, and Destroy leaves it in place.
	FindCache(ctx context.Context, key string) (bool, error)
	// Tag adds a tag to the template the VM became; a key=value tag
	// (LABEL) replaces the tags of the same key inherited from the source
	// template.
	Tag(ctx context.Context, tag string) error
	// LocalAddr returns the IP address of this machine that the VM can
	// reach: where SERVE is advertised when --addr is not set.
	LocalAddr(ctx context.Context) (string, error)
	// SendKeys types keys into the VM console, in the boot_command syntax
	// of Packer (BOOT).
	SendKeys(ctx context.Context, keys string) error
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
	// Ask asks on Stdin what to do when the build fails: retry the failed
	// instruction, clean up, or keep the VM. It requires Stdin.
	Ask bool
	// Out receives the build log.
	Out io.Writer
	// NewBuilder creates the builder of each stage; NewBuilder when nil.
	NewBuilder NewBuilderFunc
	// Dial opens connections; SSH when nil.
	Dial DialFunc
	// RetryDelay is the pause between connection attempts; 5s when zero.
	RetryDelay time.Duration
	// Stdin is where BREAKPOINT waits for Enter and Ask reads the answer.
	// When nil (no terminal), BREAKPOINT is skipped with a warning.
	Stdin io.Reader
	// Jobs is the number of stages built at the same time; 1 when zero.
	Jobs int
	// NoCache builds every stage, without looking up or storing templates
	// in the cache.
	NoCache bool
	// FindPlugin finds the Packer plugin of the packer:// stages;
	// FindPlugin when nil.
	FindPlugin FindPluginFunc
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
	// Stages running in parallel share the log; each write is a whole line.
	opts.Out = &lockedWriter{w: opts.Out}
	if opts.Jobs == 0 {
		opts.Jobs = 1
	}
	if opts.Dial == nil {
		opts.Dial = dialSSH
	}
	if opts.RetryDelay == 0 {
		opts.RetryDelay = 5 * time.Second
	}
	if opts.FindPlugin == nil {
		opts.FindPlugin = FindPlugin
	}
	if opts.NewBuilder == nil {
		opts.NewBuilder = func(s *plan.Stage, parent string) (Builder, error) { return NewBuilder(s, parent, opts.Out) }
	}
	if opts.Ask && opts.Stdin == nil {
		return errors.New("--on-error=ask needs a terminal")
	}
	var con *console
	if opts.Stdin != nil {
		con = &console{in: bufio.NewReader(opts.Stdin)}
	}
	stages, err := p.Select(opts.Target)
	if err != nil {
		return err
	}
	selected := map[*plan.Stage]bool{}
	for _, s := range stages {
		selected[s] = true
	}
	// Cache keys, computed before anything starts: a missing COPY source
	// fails early.
	keys := map[*plan.Stage]string{}
	if !opts.NoCache {
		for _, s := range stages {
			if keys[s], err = cacheKey(p.File, s, keys[s.Parent], opts.ContextDir); err != nil {
				return err
			}
		}
	}

	done := map[*plan.Stage]*executor{}
	var temporary []*executor
	defer func() {
		// A stage failure was already answered in runStage; this asks for
		// the failures before any VM exists (the builder of a stage).
		if err != nil && opts.Ask && len(temporary) > 0 && !answered(err) && ctx.Err() == nil {
			con.mu.Lock()
			fmt.Fprintf(opts.Out, "==> Build failed: %v\n", err)
			err = answer(err, ask(ctx, con.in, opts.Out, "build", false))
			con.mu.Unlock()
		}
		if err != nil && keep(err, opts) {
			for _, e := range temporary {
				e.say("Build failed, keeping intermediate template %s", e.b.ID())
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

	// A stage starts once its parent is done, up to opts.Jobs at a time, in
	// file order. After a failure, no stage starts and the running ones
	// finish: their OUTPUTs are kept.
	type result struct {
		stage *plan.Stage
		e     *executor
		err   error
	}
	results := make(chan result)
	pending := stages
	running := 0
	var errs []error
	for {
		for i := 0; len(errs) == 0 && ctx.Err() == nil && running < opts.Jobs && i < len(pending); {
			s := pending[i]
			if s.Parent != nil && done[s.Parent] == nil {
				i++
				continue
			}
			pending = append(pending[:i:i], pending[i+1:]...)
			needed := needsTemplate(s, selected)
			// Only a stage that ends as a template can be cached.
			key := keys[s]
			if !needed && s.TemplateOutput() == nil {
				key = ""
			}
			running++
			go func(parent *executor) {
				e, err := runStage(ctx, p, s, parent, needed, key, con, opts)
				results <- result{s, e, err}
			}(done[s.Parent])
		}
		if running == 0 {
			break
		}
		r := <-results
		running--
		if r.err != nil {
			errs = append(errs, r.err)
			if running > 0 {
				fmt.Fprintf(opts.Out, "==> Stage %s failed, waiting for the %d running stage(s) to finish\n", r.stage.Name(), running)
			}
			continue
		}
		done[r.stage] = r.e
		// A template in the cache stays for the next builds.
		if needsTemplate(r.stage, selected) && r.stage.TemplateOutput() == nil && !r.e.cached {
			temporary = append(temporary, r.e)
		}
	}
	switch len(errs) {
	case 0:
	case 1:
		return errs[0]
	default:
		return errors.Join(errs...)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fmt.Fprintln(opts.Out, "==> Build finished")
	return nil
}

// needsTemplate tells whether a selected stage starts from s.
func needsTemplate(s *plan.Stage, selected map[*plan.Stage]bool) bool {
	for _, c := range s.Children {
		if selected[c] {
			return true
		}
	}
	return false
}

// lockedWriter serializes the writes of the stages running in parallel.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// console is the terminal shared by the stages: one question at a time.
type console struct {
	mu sync.Mutex
	in *bufio.Reader
}

// runStage builds one stage. parent is the executor of the parent stage
// (nil for a root stage), whose final USER, SHELL, WORKDIR and ENV are
// inherited.
// needed is true when a selected stage starts from this one: the VM then
// always ends as a template. key is the stage's cache key, empty when the
// stage is not cached.
func runStage(ctx context.Context, p *plan.Plan, stage *plan.Stage, parent *executor, needed bool, key string, con *console, opts Options) (_ *executor, err error) {
	if stage.IsPacker() {
		return runPackerStage(ctx, p, stage, con, opts)
	}
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
		console:    con,
	}
	if parent != nil {
		// Like a Docker image: USER, SHELL, WORKDIR and ENV carry over, ARG
		// does not.
		e.user, e.shell, e.workdir = parent.user, parent.shell, parent.workdir
		e.env = append([]instructions.KeyValue{}, parent.env...)
	}

	if e.connect != nil && e.user == "" {
		e.user = e.connect.User
	}

	if key != "" {
		found, err := b.FindCache(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("looking up the cache: %w", err)
		}
		if found {
			e.say("Using the cached template %s (key %s)", b.ID(), key[:12])
			e.cached = true
			// The child stages inherit the final USER, SHELL, WORKDIR and ENV.
			e.replay(stage.Steps)
			if err := exportXVA(ctx, p, stage, e, opts); err != nil {
				return nil, err
			}
			e.say("Stage finished (cached)")
			return e, nil
		}
	}

	if sv := stage.Serve; sv != nil {
		addr, stop, err := e.serve(ctx, sv)
		if err != nil {
			return nil, &plan.Error{File: p.File, Line: sv.Line(), Err: err}
		}
		// Stopped last: still up while --on-error=ask waits.
		defer stop()
		e.serveAddr = addr
	}

	cloudConfig := ""
	if e.connect != nil {
		authorizedKey, err := e.generateKey()
		if err != nil {
			return nil, err
		}
		cloudConfig = CloudConfig(e.connect.User, authorizedKey)
	}

	converted := false
	defer func() {
		if converted {
			return
		}
		// Failures outside the instructions (start, connection, stop,
		// OUTPUT): no retry, but the VM can be kept.
		if err != nil && opts.Ask && !answered(err) && ctx.Err() == nil {
			err = answer(err, e.ask(ctx, err, false))
		}
		if err != nil && keep(err, opts) {
			e.say("Build failed, keeping VM %s", b.ID())
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

	for _, boot := range stage.Boot {
		keys := strings.ReplaceAll(boot.Keys, instructions.ServeAddrMarker, e.serveAddr)
		e.say("BOOT %s", keys)
		if err := b.SendKeys(ctx, keys); err != nil {
			return nil, &plan.Error{File: p.File, Line: boot.Line(), Err: fmt.Errorf("BOOT: %w", err)}
		}
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

	if err := e.steps(ctx, p, stage, opts); err != nil {
		return nil, err
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
	if err := exportXVA(ctx, p, stage, e, opts); err != nil {
		return nil, err
	}
	store := key != "" && !e.retried
	if out := stage.TemplateOutput(); out != nil {
		e.say("Converting VM %s to a template", b.ID())
		if err := b.Output(ctx, out); err != nil {
			return nil, &plan.Error{File: p.File, Line: out.Line(), Err: err}
		}
		converted = true
		// The labels before the cache tag: a cached template has them.
		for _, l := range stage.Labels {
			e.say("LABEL %s=%s", l.Key, l.Value)
			if err := b.Tag(ctx, l.Key+"="+l.Value); err != nil {
				return nil, &plan.Error{File: p.File, Line: out.Line(), Err: fmt.Errorf("LABEL %s: tagging the template: %w", l.Key, err)}
			}
		}
		if store {
			e.cached = e.tag(ctx, CacheTagPrefix+key)
		}
	} else if needed {
		prefix := "opendango-tmp-"
		if store {
			prefix = "opendango-cache-"
		}
		now := time.Now().Unix()
		name := fmt.Sprintf("%s%s-%d", prefix, stage.Name(), now)
		e.say("Converting VM %s to the intermediate template %s", b.ID(), name)
		if err := b.Output(ctx, &instructions.Output{Kind: instructions.OutputTemplate, Name: name}); err != nil {
			return nil, fmt.Errorf("converting to an intermediate template: %w", err)
		}
		converted = true
		// The creation tag first: a template with the key but without it
		// would never be pruned.
		if store {
			e.cached = e.tag(ctx, fmt.Sprintf("%s%d", CreatedTagPrefix, now)) && e.tag(ctx, CacheTagPrefix+key)
		}
	}
	e.say("Stage finished")
	return e, nil
}

// steps runs the instructions of the stage. With --on-error=ask, a failed
// instruction can be retried.
func (e *executor) steps(ctx context.Context, p *plan.Plan, stage *plan.Stage, opts Options) error {
	for _, step := range stage.Steps {
		for {
			err := e.step(ctx, step)
			if err == nil {
				break
			}
			err = &plan.Error{File: p.File, Line: step.Line(), Err: err}
			if !opts.Ask || ctx.Err() != nil {
				return err
			}
			c := e.ask(ctx, err, true)
			if c != choiceRetry {
				e.answer, e.answered = c, true
				return answer(err, c)
			}
			// What was fixed by hand before the retry is not reproducible.
			e.retried = true
			if err := e.reconnect(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// Cache tags, the same for every builder. Every template in the cache has
// CacheTagPrefix+key; the intermediate ones also have CreatedTagPrefix+<unix
// time>, which tells them from the OUTPUT templates for opendango cache
// prune.
const (
	CacheTagPrefix   = "opendango-cache:"
	CreatedTagPrefix = "opendango-cache-created:"
)

// tag tags the template and reports whether it worked. A failure only
// means that the template is not cached: an untagged intermediate template
// is deleted at the end of the build like with --no-cache.
func (e *executor) tag(ctx context.Context, tag string) bool {
	if err := e.b.Tag(ctx, tag); err != nil {
		e.say("Warning: tagging template %s failed, it is not cached: %v", e.b.ID(), err)
		return false
	}
	return true
}

// exportXVA runs the XVA OUTPUTs of the stage.
func exportXVA(ctx context.Context, p *plan.Plan, stage *plan.Stage, e *executor, opts Options) error {
	for _, out := range stage.Outputs {
		if out.Kind != instructions.OutputXVA {
			continue
		}
		o := *out
		if !filepath.IsAbs(o.Path) {
			o.Path = filepath.Join(opts.ContextDir, o.Path)
		}
		e.say("Exporting XVA to %s", o.Path)
		if err := e.b.Output(ctx, &o); err != nil {
			return &plan.Error{File: p.File, Line: out.Line(), Err: err}
		}
	}
	return nil
}

// replay applies the state changes of the steps (ARG, ENV, USER, SHELL,
// WORKDIR) without running anything: for a stage found in the cache.
func (e *executor) replay(steps []instructions.Instruction) {
	for _, step := range steps {
		switch s := step.(type) {
		case *instructions.Arg:
			if s.Set {
				e.args = setVar(e.args, s.Name, s.Value)
			}
		case *instructions.Env:
			for _, kv := range s.Vars {
				e.env = setVar(e.env, kv.Key, kv.Value)
			}
		case *instructions.User:
			e.user = s.Name
		case *instructions.Shell:
			e.shell = s.Args
		case *instructions.Workdir:
			e.workdir = e.workdirPath(s.Path)
		}
	}
}

// workdirPath joins a relative WORKDIR to the current one.
func (e *executor) workdirPath(dir string) string {
	if !path.IsAbs(dir) && e.workdir != "" {
		return path.Join(e.workdir, dir)
	}
	return dir
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
	console    *console

	key        ed25519.PrivateKey
	signer     gossh.Signer
	keyComment string
	// host is the address of the current connection; empty once the VM
	// cannot be reached anymore with the ephemeral key.
	host string

	// cached is true when the stage's template is in the cache: found
	// there, or tagged at the end of the stage.
	cached bool
	// serveAddr is the address of the SERVE server (ip:port), exported to
	// RUN as DANGO_HTTP_ADDR; empty without SERVE.
	serveAddr string
	// retried is true once --on-error=ask retried an instruction.
	retried bool
	// answer is the answer to --on-error=ask that ended the stage, when
	// answered is true.
	answer   choice
	answered bool

	// State changed by the instructions.
	user string
	// shell is the SHELL of the shell form of RUN; nil for /bin/sh -c.
	shell   []string
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
	e.key = priv
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
				e.host = host
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
	case *instructions.Shell:
		e.shell = s.Args
		return nil
	case *instructions.Workdir:
		dir := e.workdirPath(s.Path)
		e.say("WORKDIR %s", dir)
		if err := e.run(ctx, e.wrap("mkdir -p "+instructions.ShellQuote(dir))); err != nil {
			return fmt.Errorf("WORKDIR: %w", err)
		}
		e.workdir = dir
		return nil
	case *instructions.Run:
		e.say("RUN %s", firstLine(s.Script))
		script := s.Script
		if e.shell != nil && !s.Exec {
			// The exports and cd stay in /bin/sh, which then becomes the
			// SHELL with the script as its last argument.
			script = "exec " + quoteAll(e.shell) + " " + q(script)
		}
		if err := e.run(ctx, e.wrap(script)); err != nil {
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
	case *instructions.Breakpoint:
		if err := e.breakpoint(ctx, s); err != nil {
			return fmt.Errorf("BREAKPOINT: %w", err)
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
	if e.serveAddr != "" {
		fmt.Fprintf(&b, "export %s=%s\n", instructions.ServeAddrVar, instructions.ShellQuote(e.serveAddr))
	}
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
		matches, err := resolveSource(e.contextDir, pattern)
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
func resolveSource(contextDir, pattern string) ([]string, error) {
	if filepath.IsAbs(pattern) {
		return nil, fmt.Errorf("%s: sources must be relative to the Dangofile directory", pattern)
	}
	matches, err := filepath.Glob(filepath.Join(contextDir, pattern))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", pattern, err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("%s: no such file or directory", pattern)
	}
	for _, m := range matches {
		rel, err := filepath.Rel(contextDir, m)
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

// breakpoint prints the SSH command to reach the VM and waits for Enter.
func (e *executor) breakpoint(ctx context.Context, s *instructions.Breakpoint) error {
	title := "BREAKPOINT"
	if s.Message != "" {
		title += " " + s.Message
	}
	if e.console == nil {
		e.say("%s ignored: stdin is not a terminal", title)
		return nil
	}
	e.console.mu.Lock()
	defer e.console.mu.Unlock()
	e.say("%s", title)
	if e.key == nil {
		// packer://: no ephemeral key, the FROM flags hold the credentials.
		e.say("Build paused. The plugin owns the connection: log in with the credentials of the FROM flags")
	} else {
		e.say("Build paused. Connect to the VM with:")
		remove, err := e.printSSH()
		if err != nil {
			return err
		}
		defer remove()
	}
	e.say("Press Enter to continue (Ctrl+C aborts the build)")
	if _, err := readLine(ctx, e.console.in); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	e.say("Resuming the build")
	return nil
}

// printSSH writes the ephemeral key to a temporary file and prints the ssh
// command that uses it. The returned function removes the file.
func (e *executor) printSSH() (remove func(), err error) {
	block, err := gossh.MarshalPrivateKey(e.key, e.keyComment)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp("", "opendango-key-")
	if err != nil {
		return nil, err
	}
	remove = func() { os.Remove(f.Name()) }
	if _, err := f.Write(pem.EncodeToMemory(block)); err != nil {
		f.Close()
		remove()
		return nil, err
	}
	if err := f.Close(); err != nil {
		remove()
		return nil, err
	}
	fmt.Fprintf(e.out, "    ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p %d %s@%s\n",
		f.Name(), e.connect.Port, e.connect.User, e.host)
	return remove, nil
}

// readLine reads one line from in, or returns when ctx is done. The read
// cannot be interrupted: on cancellation it is left behind, and the build
// ends anyway. No read starts once ctx is done, so that two reads never
// share in.
func readLine(ctx context.Context, in *bufio.Reader) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	type result struct {
		line string
		err  error
	}
	read := make(chan result, 1)
	go func() {
		line, err := in.ReadString('\n')
		read <- result{line, err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-read:
		return r.line, r.err
	}
}

// choice is the answer to --on-error=ask.
type choice int

const (
	choiceClean choice = iota
	choiceKeep
	choiceRetry
)

// ask prints the failure and the SSH command when the VM was reached, then
// asks what to do.
func (e *executor) ask(ctx context.Context, failure error, canRetry bool) choice {
	e.console.mu.Lock()
	defer e.console.mu.Unlock()
	e.say("Build failed: %v", failure)
	if e.host != "" {
		e.say("Inspect the VM with:")
		if remove, err := e.printSSH(); err == nil {
			defer remove()
		}
	}
	return ask(ctx, e.console.in, e.out, e.name, canRetry)
}

// ask reads the answer until it is valid. End of input and Ctrl+C mean
// clean up.
func ask(ctx context.Context, in *bufio.Reader, out io.Writer, name string, canRetry bool) choice {
	prompt := "[c] clean up and exit, [k] keep the VM and exit"
	if canRetry {
		prompt = "[r] retry the instruction, " + prompt
	}
	for {
		fmt.Fprintf(out, "==> %s: %s? ", name, prompt)
		line, err := readLine(ctx, in)
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "c":
			return choiceClean
		case "k":
			return choiceKeep
		case "r":
			if canRetry {
				return choiceRetry
			}
		}
		if err != nil {
			fmt.Fprintln(out)
			return choiceClean
		}
	}
}

// answeredError is a failure the user answered with clean or keep.
type answeredError struct {
	error
	keep bool
}

func (a *answeredError) Unwrap() error { return a.error }

func answer(err error, c choice) error { return &answeredError{error: err, keep: c == choiceKeep} }

func answered(err error) bool {
	var a *answeredError
	return errors.As(err, &a)
}

// keep tells whether a failed build leaves the VM and the intermediate
// templates in place.
func keep(err error, opts Options) bool {
	var a *answeredError
	if errors.As(err, &a) {
		return a.keep
	}
	return opts.KeepOnError
}

// reconnect opens a new connection before a retry: the failure may have
// dropped the previous one (REBOOT, sshd restarted by hand).
func (e *executor) reconnect(ctx context.Context) error {
	if e.b == nil {
		// packer://: the plugin owns the connection.
		return nil
	}
	if e.comm != nil {
		e.comm.Close()
		e.comm = nil
	}
	return e.open(ctx)
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
	// The key is gone and the VM is stopping: no more SSH command to offer.
	e.host = ""
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

// quoteAll quotes each word for a POSIX shell.
func quoteAll(words []string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = q(w)
	}
	return strings.Join(quoted, " ")
}

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
