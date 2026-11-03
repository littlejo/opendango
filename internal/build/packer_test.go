package build

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
		if source != "opendangotest-local" {
			return nil, "", fmt.Errorf("unexpected source %q", source)
		}
		return &packer.Plugin{Name: "opendangotest", Path: pluginPath}, "local", nil
	}
}

func TestPackerStage(t *testing.T) {
	root := t.TempDir()
	ctxDir := t.TempDir()
	os.WriteFile(filepath.Join(ctxDir, "a.txt"), []byte("A"), 0o644)
	src := fmt.Sprintf(`FROM --root=%[1]s --count=2 --items='["x", "y"]' --password=s3cret packer://opendangotest-local AS golden
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

func TestPackerStageFailure(t *testing.T) {
	src := func(root string) string {
		return fmt.Sprintf("FROM --root=%s packer://opendangotest-local\nCONNECT ssh --user=tester\nRUN test -f marker || { touch marker; exit 3; }\n", root)
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
		// No ssh command: the plugin owns the connection.
		if !strings.Contains(log.String(), "[r] retry the instruction") || strings.Contains(log.String(), "ssh -i") {
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
