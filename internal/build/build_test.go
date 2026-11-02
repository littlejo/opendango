package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	sshcomm "github.com/littlejo/opendango/internal/communicator/ssh"
	"github.com/littlejo/opendango/internal/instructions"
	"github.com/littlejo/opendango/internal/parser"
	"github.com/littlejo/opendango/internal/plan"
)

// fakeBuilder records the calls the executor makes. It stands for the
// infrastructure in unit tests only; builders have their own integration
// tests against the real infrastructure.
type fakeBuilder struct {
	id          string
	parent      string
	calls       []string
	cloudConfig string
	outputs     []instructions.Output
}

func (b *fakeBuilder) Start(_ context.Context, cloudConfig string) error {
	b.calls = append(b.calls, "start")
	b.cloudConfig = cloudConfig
	return nil
}

func (b *fakeBuilder) Address(context.Context) (string, error) {
	b.calls = append(b.calls, "address")
	return "192.0.2.10", nil
}

func (b *fakeBuilder) Stop(_ context.Context, grace time.Duration) error {
	b.calls = append(b.calls, fmt.Sprintf("stop(%s)", grace))
	return nil
}

func (b *fakeBuilder) Output(_ context.Context, out *instructions.Output) error {
	b.calls = append(b.calls, "output:"+out.Kind)
	b.outputs = append(b.outputs, *out)
	return nil
}

func (b *fakeBuilder) Destroy(context.Context) error {
	b.calls = append(b.calls, "destroy")
	return nil
}

func (b *fakeBuilder) ID() string {
	if b.id == "" {
		return "vm-1"
	}
	return b.id
}

// only returns a NewBuilderFunc for single-stage plans.
func only(b *fakeBuilder) NewBuilderFunc {
	return func(*plan.Stage, string) (Builder, error) { return b, nil }
}

// localComm runs the commands with the local /bin/sh, in a sandbox: HOME is
// a temporary directory and sudo, shutdown and reboot are stubs that only
// log their arguments (sudo then runs the command after "--").
type localComm struct {
	home, bin, log string
}

func newLocalComm(t *testing.T) *localComm {
	t.Helper()
	dir := t.TempDir()
	c := &localComm{home: filepath.Join(dir, "home"), bin: filepath.Join(dir, "bin"), log: filepath.Join(dir, "log")}
	for _, d := range []string{c.bin, filepath.Join(c.home, ".ssh")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stubs := map[string]string{
		"sudo":     "echo \"sudo $*\" >> \"$LOG\"\nwhile [ $# -gt 0 ] && [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n",
		"shutdown": "echo \"shutdown $*\" >> \"$LOG\"\n",
		"reboot":   "echo \"reboot $*\" >> \"$LOG\"\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(c.bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func (c *localComm) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = c.home
	cmd.Env = append(os.Environ(), "HOME="+c.home, "PATH="+c.bin+":"+os.Getenv("PATH"), "LOG="+c.log)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &sshcomm.ExitError{Code: exitErr.ExitCode()}
	}
	return err
}

func (c *localComm) Upload(_ context.Context, src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func (c *localComm) Close() error { return nil }

func (c *localComm) logged(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(c.log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

// scriptComm records the commands and answers them from a script.
type scriptComm struct {
	commands []string
	// reply returns the stdout and error of a command.
	reply  func(command string) (string, error)
	closed int
}

func (c *scriptComm) Run(_ context.Context, command string, stdout, _ io.Writer) error {
	c.commands = append(c.commands, command)
	if c.reply == nil {
		return nil
	}
	out, err := c.reply(command)
	io.WriteString(stdout, out)
	return err
}

func (c *scriptComm) Upload(context.Context, string, string) error { return nil }
func (c *scriptComm) Close() error                                 { c.closed++; return nil }

func mustPlan(t *testing.T, src string) *plan.Plan {
	t.Helper()
	res, err := parser.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build("Dangofile", res, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func dialTo(c Communicator) DialFunc {
	return func(context.Context, string, int, string, gossh.Signer) (Communicator, error) { return c, nil }
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestRunLocal runs RUN, ENV, ARG, WORKDIR and COPY for real with the local
// shell and checks their effects.
func TestRunLocal(t *testing.T) {
	ctxDir := t.TempDir()
	for name, content := range map[string]string{"a.txt": "A", "b.txt": "B", "site/index.html": "<h1>", "site/css/s.css": "x"} {
		p := filepath.Join(ctxDir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	out := filepath.Join(t.TempDir(), "out")
	src := fmt.Sprintf(`FROM xo://t AS golden
CONNECT ssh --user=tester
ARG GREETING=hello
ENV TARGET="the world" GREETING=overridden
WORKDIR %[1]s
WORKDIR sub
RUN echo "$GREETING $TARGET" > env.txt && pwd > pwd.txt
RUN ["sh", "-c", "echo exec form > exec.txt"]
RUN <<EOF
echo line1 > heredoc.txt
echo line2 >> heredoc.txt
EOF
COPY a.txt %[1]s/file-copy.txt
COPY --chmod=0600 a.txt b.txt %[1]s/dir/
COPY site %[1]s/site
COPY a.txt relative/
OUTPUT --format=xva out/golden.xva
OUTPUT --as=template golden
`, out)
	p := mustPlan(t, src)
	comm := newLocalComm(t)
	b := &fakeBuilder{}
	var log bytes.Buffer
	if err := Run(context.Background(), p, Options{ContextDir: ctxDir, Out: &log, NewBuilder: only(b), Dial: dialTo(comm)}); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}

	sub := filepath.Join(out, "sub")
	checks := map[string]string{
		filepath.Join(sub, "env.txt"):           "overridden the world\n",
		filepath.Join(sub, "pwd.txt"):           sub + "\n",
		filepath.Join(sub, "exec.txt"):          "exec form\n",
		filepath.Join(sub, "heredoc.txt"):       "line1\nline2\n",
		filepath.Join(out, "file-copy.txt"):     "A",
		filepath.Join(out, "dir", "a.txt"):      "A",
		filepath.Join(out, "dir", "b.txt"):      "B",
		filepath.Join(out, "site/index.html"):   "<h1>",
		filepath.Join(out, "site/css/s.css"):    "x",
		filepath.Join(sub, "relative", "a.txt"): "A",
	}
	for path, want := range checks {
		if got := readFile(t, path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if info, err := os.Stat(filepath.Join(out, "dir", "a.txt")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("--chmod: mode = %v, %v", info.Mode(), err)
	}

	if !strings.Contains(comm.logged(t), "shutdown -h now") {
		t.Errorf("no shutdown, log: %q", comm.logged(t))
	}
	if !strings.Contains(b.cloudConfig, "name: \"tester\"") || !strings.Contains(b.cloudConfig, "ssh-ed25519 ") {
		t.Errorf("cloud-config = %q", b.cloudConfig)
	}
	wantCalls := "start address stop(5m0s) output:xva output:template"
	if got := strings.Join(b.calls, " "); got != wantCalls {
		t.Errorf("builder calls = %q, want %q", got, wantCalls)
	}
	if got := b.outputs[0].Path; got != filepath.Join(ctxDir, "out/golden.xva") {
		t.Errorf("xva path = %q", got)
	}
	if !strings.Contains(log.String(), "==> golden: RUN echo line1 > heredoc.txt ...") {
		t.Errorf("log does not show the steps:\n%s", log.String())
	}
}

// TestRemovesEphemeralKey checks that the key injected by cloud-init is
// removed from authorized_keys, and only that key.
func TestRemovesEphemeralKey(t *testing.T) {
	comm := newLocalComm(t)
	b := &fakeBuilder{}
	ak := filepath.Join(comm.home, ".ssh", "authorized_keys")
	// The builder injects the key at Start: simulate cloud-init.
	start := func(cloudConfig string) {
		for _, line := range strings.Split(cloudConfig, "\n") {
			if k, ok := strings.CutPrefix(strings.TrimSpace(line), "- \"ssh-ed25519"); ok {
				os.WriteFile(ak, []byte("ssh-rsa AAAA keep-me\nssh-ed25519"+strings.TrimSuffix(k, "\"")+"\n"), 0o600)
			}
		}
	}
	p := mustPlan(t, "FROM xo://t\nCONNECT ssh --user=tester\nRUN true\n")
	err := Run(context.Background(), p, Options{NewBuilder: only(b), Dial: func(context.Context, string, int, string, gossh.Signer) (Communicator, error) {
		start(b.cloudConfig)
		return comm, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, ak); got != "ssh-rsa AAAA keep-me\n" {
		t.Errorf("authorized_keys = %q", got)
	}
}

func TestUserRunsThroughSudo(t *testing.T) {
	comm := &scriptComm{}
	p := mustPlan(t, "FROM xo://t\nCONNECT ssh --user=debian\nRUN whoami\nUSER root\nRUN whoami\nUSER debian\nRUN whoami\n")
	if err := Run(context.Background(), p, Options{NewBuilder: only(&fakeBuilder{}), Dial: dialTo(comm)}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/bin/sh -c 'whoami'",
		"sudo -n -H -u 'root' -- /bin/sh -c 'whoami'",
		"/bin/sh -c 'whoami'",
	}
	for i, w := range want {
		if comm.commands[i] != w {
			t.Errorf("command %d = %q, want %q", i, comm.commands[i], w)
		}
	}
}

func TestEnvPersist(t *testing.T) {
	comm := &scriptComm{}
	p := mustPlan(t, "FROM xo://t\nCONNECT ssh --user=debian\nENV --persist APP=prod\nRUN true\n")
	if err := Run(context.Background(), p, Options{NewBuilder: only(&fakeBuilder{}), Dial: dialTo(comm)}); err != nil {
		t.Fatal(err)
	}
	persist := comm.commands[0]
	if !strings.HasPrefix(persist, "sudo -n -- /bin/sh -c ") || !strings.Contains(persist, "/etc/environment") || !strings.Contains(persist, "APP") {
		t.Errorf("persist command = %q", persist)
	}
	if !strings.Contains(comm.commands[1], "export APP=") {
		t.Errorf("RUN does not see the ENV: %q", comm.commands[1])
	}
}

// TestEnvPersistScript runs the generated /etc/environment script on a copy
// of the file to check its shell syntax and its effect.
func TestEnvPersistScript(t *testing.T) {
	comm := &scriptComm{}
	p := mustPlan(t, "FROM xo://t\nCONNECT ssh --user=root\nENV --persist APP=\"new value\"\n")
	if err := Run(context.Background(), p, Options{NewBuilder: only(&fakeBuilder{}), Dial: dialTo(comm)}); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(t.TempDir(), "environment")
	os.WriteFile(env, []byte("PATH=\"/usr/bin\"\nAPP=\"old\"\n"), 0o644)
	script := strings.ReplaceAll(comm.commands[0], "/etc/environment", env)
	if out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got := readFile(t, env); got != "PATH=\"/usr/bin\"\nAPP=\"new value\"\n" {
		t.Errorf("environment = %q", got)
	}
}

func TestReboot(t *testing.T) {
	bootIDs := []string{"boot-1", "boot-1", "boot-2"}
	comm := &scriptComm{reply: func(cmd string) (string, error) {
		if strings.HasPrefix(cmd, "cat /proc/sys/kernel/random/boot_id") {
			id := bootIDs[0]
			bootIDs = bootIDs[1:]
			return id + "\n", nil
		}
		if strings.Contains(cmd, "reboot") {
			return "", errors.New("connection lost")
		}
		return "", nil
	}}
	b := &fakeBuilder{}
	p := mustPlan(t, "FROM xo://t\nCONNECT ssh --user=debian\nREBOOT\nRUN true\n")
	if err := Run(context.Background(), p, Options{NewBuilder: only(b), Dial: dialTo(comm), RetryDelay: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if len(bootIDs) != 0 {
		t.Errorf("boot ID read %d times, want 3", 3-len(bootIDs))
	}
	if !contains(comm.commands, "sudo -n -- reboot") {
		t.Errorf("no reboot in %q", comm.commands)
	}
	// One address lookup per connection: initial, stale one, rebooted one.
	if got := strings.Count(strings.Join(b.calls, " "), "address"); got != 3 {
		t.Errorf("address called %d times, want 3", got)
	}
}

func TestConnectRetries(t *testing.T) {
	attempts := 0
	comm := &scriptComm{}
	dial := func(context.Context, string, int, string, gossh.Signer) (Communicator, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("connection refused")
		}
		return comm, nil
	}
	p := mustPlan(t, "FROM xo://t\nCONNECT ssh --user=debian\nRUN true\n")
	if err := Run(context.Background(), p, Options{NewBuilder: only(&fakeBuilder{}), Dial: dial, RetryDelay: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d", attempts)
	}
}

func TestConnectTimeout(t *testing.T) {
	dial := func(context.Context, string, int, string, gossh.Signer) (Communicator, error) {
		return nil, errors.New("connection refused")
	}
	b := &fakeBuilder{}
	p := mustPlan(t, "FROM xo://t\nCONNECT ssh --user=debian --timeout=20ms\nRUN true\n")
	err := Run(context.Background(), p, Options{NewBuilder: only(b), Dial: dial, RetryDelay: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("got %v", err)
	}
	if b.calls[len(b.calls)-1] != "destroy" {
		t.Errorf("VM not destroyed: %v", b.calls)
	}
}

func TestFailureDestroysOrKeeps(t *testing.T) {
	for _, keep := range []bool{false, true} {
		comm := &scriptComm{reply: func(cmd string) (string, error) {
			if strings.Contains(cmd, "false") {
				return "", &sshcomm.ExitError{Code: 3}
			}
			return "", nil
		}}
		b := &fakeBuilder{}
		p := mustPlan(t, "FROM xo://t\nCONNECT ssh --user=debian\nRUN true\nRUN false\nOUTPUT --as=template\n")
		err := Run(context.Background(), p, Options{NewBuilder: only(b), Dial: dialTo(comm), KeepOnError: keep})
		if err == nil || err.Error() != "Dangofile:4: RUN: exit code 3" {
			t.Fatalf("keep=%v: got %v", keep, err)
		}
		destroyed := b.calls[len(b.calls)-1] == "destroy"
		if destroyed == keep {
			t.Errorf("keep=%v: calls %v", keep, b.calls)
		}
	}
}

func TestDestroyWithoutTemplate(t *testing.T) {
	b := &fakeBuilder{}
	p := mustPlan(t, "FROM xo://t\nOUTPUT --format=xva /tmp/x.xva\n")
	if err := Run(context.Background(), p, Options{NewBuilder: only(b)}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(b.calls, " "); got != "start stop(0s) output:xva destroy" {
		t.Errorf("calls = %q", got)
	}
	if b.cloudConfig != "" {
		t.Errorf("cloud-config without CONNECT: %q", b.cloudConfig)
	}
}

func TestCopyRejectsOutsideContext(t *testing.T) {
	ctxDir := t.TempDir()
	for _, src := range []string{"../x", "/etc/passwd", "missing"} {
		p := mustPlan(t, "FROM xo://t\nCONNECT ssh --user=debian\nCOPY "+src+" /x\n")
		err := Run(context.Background(), p, Options{ContextDir: ctxDir, NewBuilder: only(&fakeBuilder{}), Dial: dialTo(&scriptComm{})})
		if err == nil || !strings.HasPrefix(err.Error(), "Dangofile:3: COPY: ") {
			t.Errorf("%s: got %v", src, err)
		}
	}
}

func TestCloudConfig(t *testing.T) {
	got := CloudConfig("debian", "ssh-ed25519 AAAA opendango-1")
	want := "#cloud-config\nusers:\n  - default\n  - name: \"debian\"\n    ssh_authorized_keys:\n      - \"ssh-ed25519 AAAA opendango-1\"\n"
	if got != want {
		t.Errorf("got %q", got)
	}
	if got := CloudConfig("root", "k"); !strings.Contains(got, "disable_root: false\n") {
		t.Errorf("root: %q", got)
	}
}

func TestPrefixWriter(t *testing.T) {
	var out bytes.Buffer
	w := newPrefixWriter(&out, "> ")
	io.WriteString(w, "a\nb")
	io.WriteString(w, "c\nd")
	w.Flush()
	if out.String() != "> a\n> bc\n> d\n" {
		t.Errorf("got %q", out.String())
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// stageBuilders creates one fakeBuilder per stage, with ID vm-<stage>, and
// records the calls of all of them in one ordered log.
type stageBuilders struct {
	byStage map[string]*fakeBuilder
	log     []string
}

func newStageBuilders() *stageBuilders { return &stageBuilders{byStage: map[string]*fakeBuilder{}} }

func (sb *stageBuilders) factory() NewBuilderFunc {
	return func(s *plan.Stage, parent string) (Builder, error) {
		b := &loggingBuilder{fakeBuilder: fakeBuilder{id: "vm-" + s.Name(), parent: parent}, log: &sb.log}
		sb.byStage[s.Name()] = &b.fakeBuilder
		return b, nil
	}
}

type loggingBuilder struct {
	fakeBuilder
	log *[]string
}

func (b *loggingBuilder) Output(ctx context.Context, out *instructions.Output) error {
	what := out.Kind
	if strings.HasPrefix(out.Name, "opendango-tmp-") {
		what += "(tmp)"
	}
	*b.log = append(*b.log, b.id+":output:"+what)
	return b.fakeBuilder.Output(ctx, out)
}

func (b *loggingBuilder) Destroy(ctx context.Context) error {
	*b.log = append(*b.log, b.id+":destroy")
	return b.fakeBuilder.Destroy(ctx)
}

const threeStages = `FROM xo://t AS base
CONNECT ssh --user=debian
USER root
ENV A=1
WORKDIR /opt
RUN true

FROM base AS golden
RUN echo golden
OUTPUT --format=xva g.xva

FROM golden AS app
RUN echo app
OUTPUT --as=template app
`

func TestMultiStage(t *testing.T) {
	sb := newStageBuilders()
	comm := &scriptComm{}
	p := mustPlan(t, threeStages)
	if err := Run(context.Background(), p, Options{NewBuilder: sb.factory(), Dial: dialTo(comm)}); err != nil {
		t.Fatal(err)
	}
	// Each stage starts from its parent's template.
	for stage, want := range map[string]string{"base": "", "golden": "vm-base", "app": "vm-golden"} {
		if got := sb.byStage[stage].parent; got != want {
			t.Errorf("%s starts from %q, want %q", stage, got, want)
		}
	}
	want := []string{
		"vm-base:output:template(tmp)",
		"vm-golden:output:xva",
		"vm-golden:output:template(tmp)",
		"vm-app:output:template",
		// Intermediate templates are deleted at the end, children first.
		"vm-golden:destroy",
		"vm-base:destroy",
	}
	if strings.Join(sb.log, " ") != strings.Join(want, " ") {
		t.Errorf("log =\n%q\nwant\n%q", sb.log, want)
	}
	// app inherits USER, WORKDIR and ENV from base through golden.
	last := ""
	for _, c := range comm.commands {
		if strings.Contains(c, "echo app") {
			last = c
		}
	}
	for _, want := range []string{"sudo -n -H -u 'root' --", "export A=", `cd '\''/opt'\''`} {
		if !strings.Contains(last, want) {
			t.Errorf("app RUN %q does not contain %q", last, want)
		}
	}
	// A new key per stage.
	if sb.byStage["base"].cloudConfig == sb.byStage["app"].cloudConfig {
		t.Error("stages share the same SSH key")
	}
}

func TestTarget(t *testing.T) {
	sb := newStageBuilders()
	p := mustPlan(t, threeStages)
	if err := Run(context.Background(), p, Options{Target: "golden", NewBuilder: sb.factory(), Dial: dialTo(&scriptComm{})}); err != nil {
		t.Fatal(err)
	}
	if _, built := sb.byStage["app"]; built {
		t.Error("app built with --target golden")
	}
	// golden is the last stage built: no template, so its VM is destroyed.
	want := "vm-base:output:template(tmp) vm-golden:output:xva vm-golden:destroy vm-base:destroy"
	if got := strings.Join(sb.log, " "); got != want {
		t.Errorf("log = %q, want %q", got, want)
	}

	err := Run(context.Background(), p, Options{Target: "nope", NewBuilder: sb.factory()})
	if err == nil || !strings.Contains(err.Error(), `no stage named "nope"`) {
		t.Errorf("unknown target: %v", err)
	}
}

func TestMultiStageFailure(t *testing.T) {
	src := `FROM xo://t AS base
CONNECT ssh --user=debian
RUN true

FROM base AS golden
OUTPUT --as=template golden

FROM golden AS app
RUN false
OUTPUT --as=template app
`
	for _, keep := range []bool{false, true} {
		sb := newStageBuilders()
		comm := &scriptComm{reply: func(cmd string) (string, error) {
			if strings.Contains(cmd, "false") {
				return "", &sshcomm.ExitError{Code: 1}
			}
			return "", nil
		}}
		err := Run(context.Background(), mustPlan(t, src), Options{NewBuilder: sb.factory(), Dial: dialTo(comm), KeepOnError: keep})
		if err == nil || err.Error() != "Dangofile:9: RUN: exit code 1" {
			t.Fatalf("keep=%v: got %v", keep, err)
		}
		// golden's template is a finished OUTPUT: always kept.
		want := "vm-base:output:template(tmp) vm-golden:output:template vm-app:destroy vm-base:destroy"
		if keep {
			want = "vm-base:output:template(tmp) vm-golden:output:template"
		}
		if got := strings.Join(sb.log, " "); got != want {
			t.Errorf("keep=%v: log = %q, want %q", keep, got, want)
		}
	}
}
