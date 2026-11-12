package build

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunLocalFlag runs RUN --local on this machine, in the Dangofile
// directory, with the ARG and ENV values, between RUNs in the VM.
func TestRunLocalFlag(t *testing.T) {
	ctxDir := t.TempDir()
	vm := t.TempDir()
	p := mustPlan(t, fmt.Sprintf(`FROM xo://t
CONNECT ssh --user=tester
ARG A=arg
ENV B="the env"
USER root
WORKDIR %s
RUN echo vm > vm.txt
RUN --local echo "$A $B" > local.txt && pwd > pwd.txt
OUTPUT --as=template t
`, vm))
	comm := newLocalComm(t)
	var log bytes.Buffer
	if err := Run(context.Background(), p, Options{ContextDir: ctxDir, Out: &log, NewBuilder: only(&fakeBuilder{}), Dial: dialTo(comm)}); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if got := readFile(t, filepath.Join(ctxDir, "local.txt")); got != "arg the env\n" {
		t.Errorf("local.txt = %q", got)
	}
	if got := readFile(t, filepath.Join(ctxDir, "pwd.txt")); got != ctxDir+"\n" {
		t.Errorf("pwd.txt = %q", got)
	}
	if got := readFile(t, filepath.Join(vm, "vm.txt")); got != "vm\n" {
		t.Errorf("vm.txt = %q", got)
	}
	// USER root does not apply: no sudo for the local RUN.
	if strings.Contains(comm.logged(t), "local.txt") {
		t.Errorf("sudo log: %q", comm.logged(t))
	}
	if !strings.Contains(log.String(), "RUN --local echo") {
		t.Errorf("log:\n%s", log.String())
	}
}

func TestRunRetries(t *testing.T) {
	ctxDir := t.TempDir()
	p := mustPlan(t, `FROM xo://t
CONNECT ssh --user=tester
RUN --retries=2 n=$(cat count 2>/dev/null || echo 0); echo $((n+1)) > count; [ $n -ge 2 ]
RUN --local --retries=1 n=$(cat count 2>/dev/null || echo 0); echo $((n+1)) > count; [ $n -ge 1 ]
OUTPUT --as=template t
`)
	comm := newLocalComm(t)
	var log bytes.Buffer
	opts := Options{ContextDir: ctxDir, Out: &log, NewBuilder: only(&fakeBuilder{}), Dial: dialTo(comm), RetryDelay: time.Millisecond}
	if err := Run(context.Background(), p, opts); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if got := readFile(t, filepath.Join(comm.home, "count")); got != "3\n" {
		t.Errorf("VM attempts = %q", got)
	}
	if got := readFile(t, filepath.Join(ctxDir, "count")); got != "2\n" {
		t.Errorf("local attempts = %q", got)
	}
	if !strings.Contains(log.String(), "RUN failed (exit code 1), retrying (2/2)") {
		t.Errorf("log:\n%s", log.String())
	}

	// Out of retries: the last error.
	p = mustPlan(t, "FROM xo://t\nCONNECT ssh --user=tester\nRUN --retries=1 false\n")
	log.Reset()
	opts.ContextDir = t.TempDir()
	if err := Run(context.Background(), p, opts); err == nil || !strings.Contains(err.Error(), "Dangofile:3: RUN: exit code 1") {
		t.Errorf("error = %v", err)
	}
}

func TestRunTimeout(t *testing.T) {
	p := mustPlan(t, "FROM xo://t\nCONNECT ssh --user=tester\nRUN --local --timeout=100ms --retries=1 sleep 10\n")
	var log bytes.Buffer
	start := time.Now()
	err := Run(context.Background(), p, Options{ContextDir: t.TempDir(), Out: &log, NewBuilder: only(&fakeBuilder{}), Dial: dialTo(newLocalComm(t)), RetryDelay: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "RUN --local: timeout after 100ms") {
		t.Errorf("error = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s", d)
	}
	if !strings.Contains(log.String(), "retrying (1/1)") {
		t.Errorf("log:\n%s", log.String())
	}
}

// TestFetch fetches files and directories of the VM (here, local paths
// through the local shell) into the Dangofile directory.
func TestFetch(t *testing.T) {
	ctxDir := t.TempDir()
	vm := t.TempDir()
	for name, content := range map[string]string{"log/a.log": "A", "log/sub/b.log": "B", "one.txt": "1", "two.txt": "2"} {
		p := filepath.Join(vm, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o640)
	}
	os.Symlink("a.log", filepath.Join(vm, "log", "link"))
	os.MkdirAll(filepath.Join(ctxDir, "existing"), 0o755)
	p := mustPlan(t, fmt.Sprintf(`FROM xo://t
CONNECT ssh --user=tester
FETCH %[1]s/log logs
FETCH %[1]s/one.txt renamed.txt
FETCH %[1]s/one.txt %[1]s/two.txt out/
FETCH %[1]s/two.txt existing
OUTPUT --as=template t
`, vm))
	comm := newLocalComm(t)
	var log bytes.Buffer
	if err := Run(context.Background(), p, Options{ContextDir: ctxDir, Out: &log, NewBuilder: only(&fakeBuilder{}), Dial: dialTo(comm)}); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	for name, want := range map[string]string{"logs/a.log": "A", "logs/sub/b.log": "B", "renamed.txt": "1", "out/one.txt": "1", "out/two.txt": "2", "existing/two.txt": "2"} {
		if got := readFile(t, filepath.Join(ctxDir, name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if l, err := os.Readlink(filepath.Join(ctxDir, "logs", "link")); err != nil || l != "a.log" {
		t.Errorf("link = %q, %v", l, err)
	}
	if info, err := os.Stat(filepath.Join(ctxDir, "renamed.txt")); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, %v", info.Mode(), err)
	}
	// Read as root.
	if !strings.Contains(comm.logged(t), "sudo -n -- tar -C / -cf - -- ") {
		t.Errorf("sudo log: %q", comm.logged(t))
	}

	p = mustPlan(t, fmt.Sprintf("FROM xo://t\nCONNECT ssh --user=tester\nFETCH %s/missing out\n", vm))
	err := Run(context.Background(), p, Options{ContextDir: ctxDir, Out: &log, NewBuilder: only(&fakeBuilder{}), Dial: dialTo(comm)})
	if err == nil || !strings.Contains(err.Error(), "Dangofile:3: FETCH: ") {
		t.Errorf("error = %v", err)
	}
}

// TestHostEffectsAlwaysBuilt: a stage with RUN --local or FETCH is never
// taken from the cache; its children still are.
func TestHostEffectsAlwaysBuilt(t *testing.T) {
	src := strings.Replace(cachedStages, "RUN base-step", "RUN base-step\nRUN --local true", 1)
	first := newStageBuilders()
	if err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: t.TempDir(), NewBuilder: first.factory(), Dial: dialTo(&scriptComm{})}); err != nil {
		t.Fatal(err)
	}
	if lookups := first.byStage["base"].lookups; len(lookups) != 0 {
		t.Errorf("base looked up %v", lookups)
	}
	app := first.byStage["app"]
	if len(app.lookups) != 1 || len(app.tags) != 1 {
		t.Fatalf("app lookups %v, tags %v", app.lookups, app.tags)
	}
	// The next build: base is built again, app comes from the cache.
	sb := newStageBuilders()
	sb.cache = map[string]string{app.lookups[0]: "tmpl-app"}
	if err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: t.TempDir(), NewBuilder: sb.factory(), Dial: dialTo(&scriptComm{})}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sb.byStage["base"].calls, " "); !strings.Contains(got, "start") {
		t.Errorf("base calls %q", got)
	}
	if got := strings.Join(sb.byStage["app"].calls, " "); got != "output:xva" {
		t.Errorf("app calls %q", got)
	}
}
