package plan

import (
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/littlejo/opendango/internal/instructions"
	"github.com/littlejo/opendango/internal/parser"
)

// fixedBuiltins makes Builtins return values for the test.
func fixedBuiltins(t *testing.T, values map[string]string) {
	t.Helper()
	saved := Builtins
	t.Cleanup(func() { Builtins = saved })
	Builtins = func(string) map[string]string {
		m := map[string]string{}
		for k, v := range values {
			m[k] = v
		}
		return m
	}
}

// TestBuiltins checks where the builtin variables are visible: in FROM and
// dango.yaml without declaration, in a stage once declared with ARG; and
// that --build-arg replaces one.
func TestBuiltins(t *testing.T) {
	fixedBuiltins(t, map[string]string{TimestampVar: "1794000000", DateVar: "20261106", TimeVar: "214510", BuildIDVar: "b-1", GitCommitVar: "abc1234"})
	settings, err := LoadSettings(writeSettings(t, "xenorchestra:\n  vm_name: golden-${DANGO_DATE}-${DANGO_TIME}\n"))
	if err != nil {
		t.Fatal(err)
	}
	src := "FROM --tags='[\"${DANGO_GIT_COMMIT}\"]' plugin://xenorchestra\nCONNECT ssh --user=u\nENV UNDECLARED=${DANGO_DATE}\nARG DANGO_TIMESTAMP\nRUN echo $DANGO_TIMESTAMP\nOUTPUT --as=template golden-${DANGO_TIMESTAMP}\n"
	res, err := parser.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	p, err := BuildSettings("Dangofile", res, map[string]string{TimeVar: "000000"}, settings)
	if err != nil && !strings.Contains(err.Error(), "--tags") {
		t.Fatal(err)
	}
	// tags is set by LABEL for xenorchestra: another source for the rest.
	src = strings.Replace(src, "--tags='[\"${DANGO_GIT_COMMIT}\"]'", "--vm_description=${DANGO_GIT_COMMIT}", 1)
	if res, err = parser.Parse(strings.NewReader(src)); err != nil {
		t.Fatal(err)
	}
	if p, err = BuildSettings("Dangofile", res, map[string]string{TimeVar: "000000"}, settings); err != nil {
		t.Fatal(err)
	}
	s := p.Stages[0]
	flags, _ := s.PackerFlags()
	if flags["vm_name"] != "golden-20261106-000000" || flags["vm_description"] != "abc1234" {
		t.Errorf("flags %v", flags)
	}
	if out := s.TemplateOutput(); out == nil || out.Name != "golden-1794000000" {
		t.Errorf("output %+v", out)
	}
	var env *instructions.Env
	var arg *instructions.Arg
	for _, step := range s.Steps {
		switch st := step.(type) {
		case *instructions.Env:
			env = st
		case *instructions.Arg:
			arg = st
		}
	}
	// Undeclared in the stage: empty, like a global ARG in Docker.
	if env == nil || env.Vars[0].Value != "" || arg == nil || arg.Value != "1794000000" || !arg.Set {
		t.Errorf("env %+v, arg %+v", env, arg)
	}
	if p.Builtins[TimeVar] != "000000" || p.Builtins[BuildIDVar] != "b-1" {
		t.Errorf("builtins %v", p.Builtins)
	}
	// The --build-arg of a builtin is used: no warning.
	for _, w := range p.Warnings {
		if strings.Contains(w, TimeVar) {
			t.Errorf("warning %q", w)
		}
	}
}

func TestGitCommit(t *testing.T) {
	dir := t.TempDir()
	if got := gitCommit(dir); got != "" {
		t.Errorf("outside a repository: %q", got)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("commit", "-q", "--allow-empty", "-m", "x")
	if got := gitCommit(dir); len(got) < 7 {
		t.Errorf("commit %q", got)
	}
	if !reflect.DeepEqual(Volatile, []string{TimestampVar, DateVar, TimeVar, BuildIDVar}) {
		t.Errorf("volatile %v", Volatile)
	}
}
