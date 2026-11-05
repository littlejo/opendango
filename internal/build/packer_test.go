package build

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/littlejo/opendango/builders/packer"
)

var (
	pluginOnce sync.Once
	pluginPath string
	pluginErr  error
)

// testPlugin builds packer-plugin-opendangotest once and returns a
// FindPluginFunc that always finds it. Its "local" builder runs the
// instructions with the local /bin/sh, in the directory set by --root.
func testPlugin(t *testing.T) FindPluginFunc {
	t.Helper()
	pluginOnce.Do(func() {
		dir, err := os.MkdirTemp("", "opendango-plugin-")
		if err != nil {
			pluginErr = err
			return
		}
		pluginPath = filepath.Join(dir, "packer-plugin-opendangotest")
		cmd := exec.Command("go", "build", "-o", pluginPath, "./testdata/packer-plugin-opendangotest")
		cmd.Dir = "../../builders/packer"
		if out, err := cmd.CombinedOutput(); err != nil {
			pluginErr = errors.New(string(out))
		}
	})
	if pluginErr != nil {
		t.Fatalf("building the test plugin: %v", pluginErr)
	}
	return func(_ context.Context, source string) (*packer.Plugin, string, error) {
		// The tests of the table of known builders use the test plugin
		// as plugin://xenorchestra.
		if source != "opendangotest-local" && source != "xenorchestra" {
			return nil, "", fmt.Errorf("unexpected source %q", source)
		}
		return &packer.Plugin{Name: "opendangotest", Path: pluginPath}, "local", nil
	}
}

func TestPackerStage(t *testing.T) {
	root := t.TempDir()
	ctxDir := t.TempDir()
	os.WriteFile(filepath.Join(ctxDir, "a.txt"), []byte("A"), 0o644)
	src := fmt.Sprintf(`FROM --root=%[1]s --count=2 --items='["x", "y"]' --password=s3cret plugin://opendangotest-local AS golden
CONNECT ssh --user=tester --port=2222
ENV GREETING=hello
WORKDIR %[1]s/work
RUN echo "$GREETING" > greeting.txt
COPY a.txt copied/
`, root)
	var log syncBuffer
	err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: ctxDir, Out: &log, FindPlugin: testPlugin(t)})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	for p, want := range map[string]string{"work/greeting.txt": "hello\n", "work/copied/a.txt": "A"} {
		if got := readFile(t, filepath.Join(root, p)); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	for _, want := range []string{
		// The FROM flags and CONNECT, as the plugin decoded them.
		"==> golden: config: build=golden type=opendangotest-local on_error=cleanup count=2 items=x,y communicator=ssh ssh=tester:2222 timeout=10m0s",
		"==> golden: Warning: this is a test plugin",
		"==> golden: RUN echo \"$GREETING\" > greeting.txt",
		"==> golden: Artifact: local machine at " + root,
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, log.String())
		}
	}
}

// TestPackerStageServe checks that SERVE becomes the plugin's
// http_directory and port range, and BOOT its boot_command.
func TestPackerStageServe(t *testing.T) {
	ctxDir := t.TempDir()
	os.Mkdir(filepath.Join(ctxDir, "http"), 0o755)
	tests := []struct{ serve, want string }{
		{"SERVE --port=8123 http\nBOOT <esc>url=http://${DANGO_HTTP_ADDR}/ks.cfg<enter>\nBOOT <wait5>", "==> stage-0: boot_command: <esc>url=http://{{ .HTTPIP }}:{{ .HTTPPort }}/ks.cfg<enter>\n==> stage-0: boot_command: <wait5>\n==> stage-0: http: dir=" + filepath.Join(ctxDir, "http") + " ports=8123-8123"},
		{"SERVE http", "==> stage-0: http: dir=" + filepath.Join(ctxDir, "http") + " ports=0-0"},
	}
	for _, tt := range tests {
		src := fmt.Sprintf("FROM --root=%s plugin://opendangotest-local\n%s\n", t.TempDir(), tt.serve)
		var log syncBuffer
		if err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: ctxDir, Out: &log, FindPlugin: testPlugin(t)}); err != nil {
			t.Fatalf("%s: %v\n%s", tt.serve, err, log.String())
		}
		if !strings.Contains(log.String(), tt.want) {
			t.Errorf("%s: log lacks %q:\n%s", tt.serve, tt.want, log.String())
		}
	}

	src := fmt.Sprintf("FROM --root=%s plugin://opendangotest-local\nSERVE nope\n", t.TempDir())
	err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: ctxDir, FindPlugin: testPlugin(t)})
	if err == nil || err.Error() != "Dangofile:2: SERVE: nope is not a directory" {
		t.Errorf("err = %v", err)
	}
}

func TestPackerStageFailure(t *testing.T) {
	src := func(root string) string {
		return fmt.Sprintf("FROM --root=%s plugin://opendangotest-local\nCONNECT ssh --user=tester\nRUN test -f marker || { touch marker; exit 3; }\n", root)
	}

	t.Run("abort", func(t *testing.T) {
		err := Run(context.Background(), mustPlan(t, src(t.TempDir())), Options{FindPlugin: testPlugin(t)})
		if err == nil || err.Error() != "Dangofile:3: RUN: exit code 3" {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("keep", func(t *testing.T) {
		var log syncBuffer
		err := Run(context.Background(), mustPlan(t, src(t.TempDir())), Options{Out: &log, KeepOnError: true, FindPlugin: testPlugin(t)})
		if err == nil || !strings.Contains(log.String(), "on_error=abort") {
			t.Errorf("err = %v, log:\n%s", err, log.String())
		}
	})

	t.Run("ask, retry", func(t *testing.T) {
		var log syncBuffer
		err := Run(context.Background(), mustPlan(t, src(t.TempDir())), Options{Out: &log, Ask: true, Stdin: strings.NewReader("r\n"), FindPlugin: testPlugin(t)})
		if err != nil {
			t.Fatalf("%v\n%s", err, log.String())
		}
		// The ssh command of the plugin's connection (its hook data).
		if !strings.Contains(log.String(), "[r] retry the instruction") || !regexp.MustCompile(`ssh -i \S+ .* -p 22 tester@127\.0\.0\.1\n`).MatchString(log.String()) {
			t.Errorf("log:\n%s", log.String())
		}
	})

	t.Run("ask, keep", func(t *testing.T) {
		var log syncBuffer
		err := Run(context.Background(), mustPlan(t, src(t.TempDir())), Options{Out: &log, Ask: true, Stdin: strings.NewReader("k\n"), FindPlugin: testPlugin(t)})
		if err == nil || err.Error() != "Dangofile:3: RUN: exit code 3" {
			t.Errorf("err = %v", err)
		}
		// The plugin asks too: k means abort without cleanup ("a").
		if !strings.Contains(log.String(), "==> stage-0: cleanup answer: a") {
			t.Errorf("log:\n%s", log.String())
		}
	})
}

// TestPackerKnownBuilder checks that OUTPUT and LABEL become the settings of
// a builder of the table, and that the artifact is deleted without OUTPUT
// --as=template.
func TestPackerKnownBuilder(t *testing.T) {
	for _, tt := range []struct {
		name, lines, want string
		destroyed         bool
	}{
		{"template", "LABEL team=web stage=golden\nOUTPUT --as=template golden\n", "artifact: template_name=golden tags=team=web,stage=golden xva= compress=", false},
		{"unnamed template", "OUTPUT --as=template\n", "", false},
		{"xva only", "OUTPUT --format=xva --compress=zstd out/golden.xva\n", "artifact: template_name= tags= xva=CTX/out/golden.xva compress=zstd", true},
		{"no output", "", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			ctxDir := t.TempDir()
			src := fmt.Sprintf("FROM --root=%s plugin://xenorchestra\nCONNECT ssh --user=tester\nRUN true\n%s", root, tt.lines)
			var log syncBuffer
			if err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: ctxDir, Out: &log, FindPlugin: testPlugin(t)}); err != nil {
				t.Fatalf("%v\n%s", err, log.String())
			}
			want := strings.ReplaceAll(tt.want, "CTX", ctxDir)
			if got := strings.Contains(log.String(), "==> stage-0: artifact: "); (want != "") != got || (want != "" && !strings.Contains(log.String(), want)) {
				t.Errorf("want %q in:\n%s", want, log.String())
			}
			_, err := os.Stat(filepath.Join(root, "artifact-destroyed"))
			if destroyed := err == nil; destroyed != tt.destroyed {
				t.Errorf("artifact destroyed %v, want %v\n%s", destroyed, tt.destroyed, log.String())
			}
			if tt.destroyed && !strings.Contains(log.String(), "Deleting the artifact: no OUTPUT --as=template") {
				t.Errorf("log:\n%s", log.String())
			}
		})
	}
}

// TestPackerMultiStage checks FROM <packer stage> with a builder of the
// table: the child starts from the parent's artifact and inherits USER,
// WORKDIR and ENV; a parent without OUTPUT --as=template is an intermediate
// template, deleted at the end of the build through its plugin.
func TestPackerMultiStage(t *testing.T) {
	base, app, out := t.TempDir(), t.TempDir(), t.TempDir()
	src := fmt.Sprintf(`FROM --root=%[1]s plugin://xenorchestra AS base
CONNECT ssh --user=tester
ENV GREETING=hello
WORKDIR %[1]s
RUN true

FROM --root=%[2]s base AS app
RUN test "$GREETING" = hello && pwd > %[3]s/pwd
OUTPUT --as=template app
`, base, app, out)
	var log syncBuffer
	if err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: t.TempDir(), Out: &log, FindPlugin: testPlugin(t)}); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	for _, want := range []string{
		"==> base: artifact: template_name=opendango-tmp-base-",
		// The test plugin's artifact id is its root.
		"==> app: source: template=" + base + "\n",
		"==> app: artifact: template_name=app ",
		"==> base: Deleting intermediate template " + base + "\n",
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, log.String())
		}
	}
	if got := readFile(t, filepath.Join(out, "pwd")); got != base+"\n" {
		t.Errorf("WORKDIR in app: %q", got)
	}
	// The intermediate template is deleted at the end, the OUTPUT one stays.
	if _, err := os.Stat(filepath.Join(base, "artifact-destroyed")); err != nil {
		t.Errorf("base: %v", err)
	}
	if _, err := os.Stat(filepath.Join(app, "artifact-destroyed")); err == nil {
		t.Error("app: its OUTPUT template was deleted")
	}

	// --target base: nobody starts from base, its artifact goes at once.
	base = t.TempDir()
	src = fmt.Sprintf("FROM --root=%s plugin://xenorchestra AS base\nCONNECT ssh --user=tester\nRUN true\nFROM --root=%s base AS app\nRUN true\n", base, app)
	var log2 syncBuffer
	if err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: t.TempDir(), Out: &log2, Target: "base", FindPlugin: testPlugin(t)}); err != nil {
		t.Fatalf("%v\n%s", err, log2.String())
	}
	if !strings.Contains(log2.String(), "==> base: Deleting the artifact: no OUTPUT --as=template") || strings.Contains(log2.String(), "intermediate") {
		t.Errorf("log:\n%s", log2.String())
	}
}

// TestPackerServeAndBoot checks SERVE and BOOT with a builder of the table:
// RUN gets DANGO_HTTP_ADDR from the plugin's hook data, SERVE --addr sets
// http_ip, and BOOT types without boot_wait.
func TestPackerServeAndBoot(t *testing.T) {
	root, out, ctxDir := t.TempDir(), t.TempDir(), t.TempDir()
	os.Mkdir(filepath.Join(ctxDir, "http"), 0o755)
	src := fmt.Sprintf("FROM --root=%s plugin://xenorchestra\nSERVE --addr=198.51.100.7 http\nBOOT <enter>\nCONNECT ssh --user=tester\nRUN echo \"$DANGO_HTTP_ADDR\" > %s/addr\n", root, out)
	var log syncBuffer
	if err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: ctxDir, Out: &log, FindPlugin: testPlugin(t)}); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if got := readFile(t, filepath.Join(out, "addr")); got != "198.51.100.7:8080\n" {
		t.Errorf("DANGO_HTTP_ADDR = %q\n%s", got, log.String())
	}
	if !strings.Contains(log.String(), "==> stage-0: boot_wait: -1s\n") {
		t.Errorf("log:\n%s", log.String())
	}
}

// TestPackerBreakpoint checks that BREAKPOINT prints the ssh command of the
// plugin's connection, with its key in a file removed afterwards.
func TestPackerBreakpoint(t *testing.T) {
	src := fmt.Sprintf("FROM --root=%s plugin://opendangotest-local\nCONNECT ssh --user=tester\nBREAKPOINT look\n", t.TempDir())
	var log syncBuffer
	if err := Run(context.Background(), mustPlan(t, src), Options{Out: &log, Stdin: strings.NewReader("\n"), FindPlugin: testPlugin(t)}); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	m := regexp.MustCompile(`ssh -i (\S+) .* -p 22 tester@127\.0\.0\.1\n`).FindStringSubmatch(log.String())
	if m == nil {
		t.Fatalf("log:\n%s", log.String())
	}
	if _, err := os.Stat(m[1]); !os.IsNotExist(err) {
		t.Errorf("key file %s left: %v", m[1], err)
	}
}
