package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate", "-f", "../../examples/debian-nginx/Dangofile"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "is valid") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestValidateReportsLine(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"validate", "-f", "../../internal/plan/testdata/valid.dango", "--build-arg", "DEBIAN=13"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	code = run([]string{"validate", "-f", "testdata/invalid.dango"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "testdata/invalid.dango:3: EXPOSE has no meaning for a VM image") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 {
		t.Errorf("no args: exit %d", code)
	}
	if code := run([]string{"build", "--on-error=never"}, &stdout, &stderr); code != 2 {
		t.Errorf("--on-error=never: exit %d", code)
	}
	// go test runs without a terminal on stdin.
	stderr.Reset()
	if code := run([]string{"build", "--on-error=ask"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--on-error=ask needs a terminal") {
		t.Errorf("--on-error=ask: exit %d, stderr %q", code, stderr.String())
	}
	if code := run([]string{"build", "-j", "0"}, &stdout, &stderr); code != 2 {
		t.Errorf("-j 0: exit %d", code)
	}
	if code := run([]string{"cache"}, &stdout, &stderr); code != 2 {
		t.Errorf("cache: exit %d", code)
	}
	if code := run([]string{"cache", "prune", "--older-than=soon"}, &stdout, &stderr); code != 2 {
		t.Errorf("cache prune --older-than=soon: exit %d", code)
	}
	for _, args := range [][]string{{"plugin"}, {"plugin", "search"}, {"plugin", "search", "a", "b"}, {"plugin", "pull"}, {"plugin", "rm"}, {"plugin", "list", "x"}, {"plugin", "nope"}, {"plugin", "describe"}, {"plugin", "describe", "a", "b"}} {
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Errorf("%q: exit %d", args, code)
		}
	}
	if code := run([]string{"nope"}, &stdout, &stderr); code != 2 {
		t.Errorf("unknown command: exit %d", code)
	}
}

func TestFmt(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "Dangofile")
	const messy, clean = "from xo://t\nreboot  \n", "FROM xo://t\nREBOOT\n"
	if err := os.WriteFile(file, []byte(messy), 0o640); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"fmt", "-f", file}, &stdout, &stderr); code != 0 || stdout.String() != clean {
		t.Errorf("fmt: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if code := run([]string{"fmt", "--check", "-f", file}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "is not formatted") {
		t.Errorf("--check on a messy file: exit %d, stderr %q", code, stderr.String())
	}
	if code := run([]string{"fmt", "-w", "-f", file}, &stdout, &stderr); code != 0 {
		t.Errorf("-w: exit %d, stderr %q", code, stderr.String())
	}
	if got, _ := os.ReadFile(file); string(got) != clean {
		t.Errorf("-w wrote %q", got)
	}
	if info, _ := os.Stat(file); info.Mode().Perm() != 0o640 {
		t.Errorf("-w changed the mode to %v", info.Mode().Perm())
	}
	if code := run([]string{"fmt", "--check", "-f", file}, &stdout, &stderr); code != 0 {
		t.Errorf("--check on a clean file: exit %d", code)
	}
	if code := run([]string{"fmt", "-w", "--check", "-f", file}, &stdout, &stderr); code != 2 {
		t.Errorf("-w --check: exit %d", code)
	}
}

func TestPlugin(t *testing.T) {
	config := t.TempDir()
	t.Setenv("PACKER_CONFIG_DIR", config)
	t.Setenv("PACKER_PLUGIN_PATH", "")
	t.Setenv("PATH", "")
	bin := filepath.Join(config, "plugins/github.com/hashicorp/qemu/packer-plugin-qemu_v1.1.8_x5.0_"+runtime.GOOS+"_"+runtime.GOARCH)
	os.MkdirAll(filepath.Dir(bin), 0o755)
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"plugin", "list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("list: exit %d: %s", code, stderr.String())
	}
	if want := "NAME  VERSION  PATH\nqemu  1.1.8    " + bin + "\n"; stdout.String() != want {
		t.Errorf("list:\n%s\nwant:\n%s", stdout.String(), want)
	}

	// Offline: an invalid reference fails before any request.
	stdout.Reset()
	if code := run([]string{"plugin", "pull", "Qemu"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), `opendango plugin pull Qemu: invalid plugin name "Qemu"`) {
		t.Errorf("pull Qemu: exit %d, stderr %q", code, stderr.String())
	}

	if code := run([]string{"plugin", "rm", "github.com/hashicorp/qemu@1.1.8"}, &stdout, &stderr); code != 0 || stdout.String() != "Removed "+bin+"\n" {
		t.Errorf("rm: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(bin); !os.IsNotExist(err) {
		t.Errorf("rm left %s: %v", bin, err)
	}
}

// TestValidatePlugins checks that validate reports a plugin that is not
// installed as a warning, and that --offline does not look for it.
func TestValidatePlugins(t *testing.T) {
	t.Setenv("PACKER_CONFIG_DIR", t.TempDir())
	t.Setenv("PACKER_PLUGIN_PATH", "")
	t.Setenv("PATH", "")
	f := filepath.Join(t.TempDir(), "Dangofile")
	if err := os.WriteFile(f, []byte("FROM --memory=2048 plugin://qemu\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate", "-f", f}, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "warning: stage stage-0: plugin://qemu is not checked: plugin: no plugin for \"qemu\"") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"validate", "--offline", "-f", f}, &stdout, &stderr); code != 0 || strings.Contains(stderr.String(), "not checked") {
		t.Errorf("--offline: exit %d, stderr %q", code, stderr.String())
	}
}

// TestInit checks opendango init with the test plugin: every setting,
// commented out with its type, but those opendango sets; an existing file
// needs --force; validate then reads it.
func TestInit(t *testing.T) {
	bin := t.TempDir()
	cmd := exec.Command("go", "build", "-o", filepath.Join(bin, "packer-plugin-opendangotest"), "./testdata/packer-plugin-opendangotest")
	cmd.Dir = "../../builders/packer"
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the test plugin: %v\n%s", err, out)
	}
	t.Setenv("PACKER_CONFIG_DIR", t.TempDir())
	t.Setenv("PACKER_PLUGIN_PATH", bin)
	dir := t.TempDir()
	dangofile := filepath.Join(dir, "Dangofile")
	os.WriteFile(dangofile, []byte("FROM plugin://opendangotest-local\nCONNECT ssh --user=tester\nRUN true\n"), 0o644)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"init", "-f", dangofile}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	yaml := readTestFile(t, filepath.Join(dir, "dango.yaml"))
	for _, want := range []string{"\nopendangotest-local:\n", `  # root: ""`, "# string\n", "  # count: 0", "# number\n", "  # items: []", "# list of string\n"} {
		if !strings.Contains(yaml, want) {
			t.Errorf("dango.yaml lacks %q:\n%s", want, yaml)
		}
	}
	// Set by CONNECT, by opendango, or a secret.
	for _, unwanted := range []string{"ssh_username", "packer_build_name", "password"} {
		if strings.Contains(yaml, unwanted) {
			t.Errorf("dango.yaml has %q:\n%s", unwanted, yaml)
		}
	}
	if code := run([]string{"init", "-f", dangofile}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "exists (--force overwrites it)") {
		t.Errorf("second init: exit %d, %s", code, stderr.String())
	}

	// Commented out: the plugin misses root. Set in dango.yaml: valid.
	stderr.Reset()
	if code := run([]string{"validate", "-f", dangofile}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "root is required") {
		t.Errorf("validate: exit %d, %s", code, stderr.String())
	}
	os.WriteFile(filepath.Join(dir, "dango.yaml"), []byte("opendangotest-local:\n  root: "+dir+"\n"), 0o644)
	stderr.Reset()
	if code := run([]string{"validate", "-f", dangofile}, &stdout, &stderr); code != 0 {
		t.Errorf("validate with root: exit %d, %s", code, stderr.String())
	}
	// Given sources, another file.
	other := filepath.Join(dir, "other.yaml")
	if code := run([]string{"init", "--config", other, "plugin://opendangotest-local"}, &stdout, &stderr); code != 0 || !strings.Contains(readTestFile(t, other), "opendangotest-local:") {
		t.Errorf("init --config: exit %d, %s", code, stderr.String())
	}
}

func readTestFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBuildArgs(t *testing.T) {
	t.Setenv("FROM_ENV", "env")
	dir := t.TempDir()
	file := filepath.Join(dir, "args")
	content := "# comment\n\nA=file\nB=file=with=equals\nFROM_ENV\nUNSET_IN_ENV\n"
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &commonFlags{buildArgs: buildArgs{}, buildArgFiles: stringList{file}}
	for _, s := range []string{"A=flag", "FROM_ENV", "UNSET_IN_ENV"} {
		if err := c.buildArgs.Set(s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.allBuildArgs()
	if err != nil {
		t.Fatal(err)
	}
	want := buildArgs{"A": "flag", "B": "file=with=equals", "FROM_ENV": "env"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if err := c.buildArgs.Set("=x"); err == nil {
		t.Error("an empty name is accepted")
	}
	os.WriteFile(file, []byte("=bad\n"), 0o644)
	if _, err := c.allBuildArgs(); err == nil || !strings.Contains(err.Error(), file+":1:") {
		t.Errorf("error = %v", err)
	}
}
