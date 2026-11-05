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
	"time"

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
		// The post-processor exports the artifact, then it is deleted.
		{"xva only", "OUTPUT --format=xva --compress=zstd out/golden.xva\nOUTPUT --format=xva other.xva\n", "", true},
		{"no output", "", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			ctxDir := t.TempDir()
			src := fmt.Sprintf("FROM --root=%s plugin://xenorchestra\nCONNECT ssh --user=tester\nRUN true\n%s", root, tt.lines)
			var log syncBuffer
			if err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: ctxDir, Out: &log, FindPlugin: testPlugin(t), NoCache: true}); err != nil {
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
			if tt.name == "xva only" {
				for p, want := range map[string]string{"out/golden.xva": "xva of " + root + " compress=zstd", "other.xva": "xva of " + root + " compress="} {
					if got := readFile(t, filepath.Join(ctxDir, p)); got != want {
						t.Errorf("%s = %q, want %q", p, got, want)
					}
				}
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
	if err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: t.TempDir(), Out: &log, FindPlugin: testPlugin(t), NoCache: true}); err != nil {
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
	if err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: t.TempDir(), Out: &log2, Target: "base", FindPlugin: testPlugin(t), NoCache: true}); err != nil {
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

// TestPackerReboot checks REBOOT through the plugin's communicator: reboot
// as root, then the boot ID until it changes. The test plugin's reboot is
// a stub that changes the boot ID.
func TestPackerReboot(t *testing.T) {
	root := t.TempDir()
	src := fmt.Sprintf("FROM --root=%s plugin://opendangotest-local\nCONNECT ssh --user=tester\nRUN cat /proc/sys/kernel/random/boot_id > before\nREBOOT\nRUN cat /proc/sys/kernel/random/boot_id > after\n", root)
	var log syncBuffer
	if err := Run(context.Background(), mustPlan(t, src), Options{Out: &log, RetryDelay: time.Millisecond, FindPlugin: testPlugin(t)}); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if before, after := readFile(t, filepath.Join(root, "before")), readFile(t, filepath.Join(root, "after")); before == after {
		t.Errorf("boot ID %q before and after REBOOT", before)
	}
	if got := readFile(t, filepath.Join(root, ".stub-log")); got != "reboot \n" {
		t.Errorf("stub log %q", got)
	}
}

// TestPackerCache checks the cache of plugin:// stages with a builder of the
// table: the test plugin's data source finds the artifacts by their tags in
// a store. First build: everything is built and tagged; second: everything
// comes from the cache; third: the child changes and starts from the cached
// parent.
func TestPackerCache(t *testing.T) {
	store := t.TempDir()
	t.Setenv("OPENDANGO_TEST_STORE", store)
	base, app := t.TempDir(), t.TempDir()
	src := func(run string) string {
		return fmt.Sprintf("FROM --root=%s plugin://xenorchestra AS base\nCONNECT ssh --user=tester\nENV FROM_BASE=yes\nRUN true\n\nFROM --root=%s base AS app\nRUN %s\nOUTPUT --format=xva app.xva\nOUTPUT --as=template app\n", base, app, run)
	}
	ctxDir := t.TempDir()
	build := func(src string) string {
		t.Helper()
		os.Remove(filepath.Join(ctxDir, "app.xva"))
		var log syncBuffer
		if err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: ctxDir, Out: &log, FindPlugin: testPlugin(t)}); err != nil {
			t.Fatalf("%v\n%s", err, log.String())
		}
		return log.String()
	}

	log := build(src(`test "$FROM_BASE" = yes`))
	for _, want := range []string{
		"==> base: artifact: template_name=opendango-cache-base-",
		" tags=opendango-cache-created:",
		",opendango-cache:",
		"==> app: artifact: template_name=app tags=opendango-cache:",
		"==> base: drop_inherited_tags: opendango-cache:,opendango-cache-created:",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("first build: log lacks %q:\n%s", want, log)
		}
	}
	// A cached intermediate template stays.
	if strings.Contains(log, "Deleting intermediate template") {
		t.Errorf("first build deleted the cached template:\n%s", log)
	}

	log = build(src(`test "$FROM_BASE" = yes`))
	if !strings.Contains(log, "==> base: Using the cached template "+base) || !strings.Contains(log, "==> app: Using the cached template "+app) || strings.Contains(log, "Running the instructions") {
		t.Errorf("second build:\n%s", log)
	}
	// The XVA is exported from the cached template.
	if got := readFile(t, filepath.Join(ctxDir, "app.xva")); got != "xva of "+app+" compress=" {
		t.Errorf("app.xva = %q", got)
	}

	// The child changes: it starts from the cached parent, whose ENV it
	// inherits (replayed).
	log = build(src(`test "$FROM_BASE" = yes && true`))
	if !strings.Contains(log, "==> base: Using the cached template "+base) || !strings.Contains(log, "==> app: source: template="+base+"\n") || !strings.Contains(log, "==> app: Running the instructions") {
		t.Errorf("third build:\n%s", log)
	}
}

// TestPackerCacheNoRetry checks that --on-error=ask does not offer retry in
// a plugin:// stage that the plugin tags for the cache once built.
func TestPackerCacheNoRetry(t *testing.T) {
	t.Setenv("OPENDANGO_TEST_STORE", t.TempDir())
	src := fmt.Sprintf("FROM --root=%s plugin://xenorchestra\nCONNECT ssh --user=tester\nRUN false\nOUTPUT --as=template x\n", t.TempDir())
	var log syncBuffer
	err := Run(context.Background(), mustPlan(t, src), Options{Out: &log, Ask: true, Stdin: strings.NewReader("c\n"), FindPlugin: testPlugin(t)})
	if err == nil || !strings.Contains(log.String(), "no retry (use --no-cache to retry)") || strings.Contains(log.String(), "[r] retry") {
		t.Errorf("err = %v, log:\n%s", err, log.String())
	}
}

// TestCheckPlugins checks the FROM flags with the test plugin: a typo with
// its suggestion, the plugin's Prepare errors and warnings, a plugin that is
// not installed, and a stage that starts from another one.
func TestCheckPlugins(t *testing.T) {
	root := t.TempDir()
	find := testPlugin(t)
	for _, tt := range []struct {
		name, src, err, warning string
	}{
		{"valid", "FROM --root=" + root + " --count=2 plugin://opendangotest-local\n", "", "this is a test plugin"},
		{"typo", "FROM --root=" + root + " --cuont=2 plugin://opendangotest-local\n", `Dangofile:1: plugin://opendangotest-local has no setting "cuont", did you mean --count?`, ""},
		{"unknown", "FROM --root=" + root + " --zzzzzz=2 plugin://opendangotest-local\n", `Dangofile:1: plugin://opendangotest-local has no setting "zzzzzz" (opendango plugin describe opendangotest-local lists them)`, ""},
		{"prepare", "FROM --count=2 plugin://opendangotest-local\n", "Dangofile:1: plugin://opendangotest-local: root is required", ""},
		{"not installed", "FROM --x=1 plugin://nothing-here\n", "", "stage stage-0: plugin://nothing-here is not checked: unexpected source"},
		// The child gets a placeholder for its parent's template.
		{"child", "FROM --root=" + root + " plugin://xenorchestra AS base\nFROM --root=" + root + " base AS app\n", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			warnings, err := CheckPlugins(context.Background(), mustPlan(t, tt.src), t.TempDir(), find)
			if (tt.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.err)) {
				t.Errorf("err = %v, want %q", err, tt.err)
			}
			if tt.warning != "" && !strings.Contains(strings.Join(warnings, "\n"), tt.warning) {
				t.Errorf("warnings %q lack %q", warnings, tt.warning)
			}
		})
	}
}
