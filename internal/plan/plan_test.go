package plan

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/littlejo/opendango/internal/instructions"
	"github.com/littlejo/opendango/internal/parser"
)

func build(t *testing.T, src string, buildArgs map[string]string) (*Plan, error) {
	t.Helper()
	res, err := parser.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return Build("Dangofile", res, buildArgs)
}

func TestValidFile(t *testing.T) {
	src, err := os.ReadFile("testdata/valid.dango")
	if err != nil {
		t.Fatal(err)
	}
	p, err := build(t, string(src), map[string]string{"DEBIAN": "13"})
	if err != nil {
		t.Fatal(err)
	}
	s := p.Stage
	if s.From.Source != "debian-13-cloud" || s.From.Name != "golden" {
		t.Errorf("FROM = %+v", s.From)
	}
	if s.Connect == nil || s.Connect.User != "debian" {
		t.Errorf("CONNECT = %+v", s.Connect)
	}
	var kinds []string
	for _, step := range s.Steps {
		kinds = append(kinds, strings.TrimPrefix(reflect.TypeOf(step).String(), "*instructions."))
	}
	want := []string{"Arg", "Arg", "User", "Env", "Env", "Workdir", "Run", "Run", "Copy", "Reboot"}
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("steps = %v, want %v", kinds, want)
	}
	if len(s.Outputs) != 2 || s.Outputs[0].Name != "golden-debian-13" || s.Outputs[1].Path != "./dist/golden-13.xva" {
		t.Errorf("outputs = %+v %+v", s.Outputs[0], s.Outputs[1])
	}
	if len(p.Warnings) != 0 {
		t.Errorf("warnings = %v", p.Warnings)
	}
}

// TestArgScope checks the Docker semantics: a global ARG is only visible in
// FROM unless the stage declares it again.
func TestArgScope(t *testing.T) {
	src := `ARG V=global
ARG ONLY_GLOBAL=g
FROM xo://t-${V}
CONNECT ssh --user=u
ARG V
ARG LOCAL=l-${V}
OUTPUT --as=template "${V}-${ONLY_GLOBAL}-${LOCAL}"
`
	p, err := build(t, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Stage.From.Source; got != "t-global" {
		t.Errorf("FROM source = %q", got)
	}
	if got := p.Stage.Outputs[0].Name; got != "global--l-global" {
		t.Errorf("output name = %q", got)
	}
	args := p.Stage.Steps
	if a := args[0].(*instructions.Arg); a.Value != "global" || !a.Set {
		t.Errorf("stage ARG V = %+v", a)
	}

	p, err = build(t, src, map[string]string{"V": "cli", "LOCAL": "c"})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Stage.Outputs[0].Name; got != "cli--c" {
		t.Errorf("with --build-arg: output name = %q", got)
	}
}

func TestEnvOverridesArgInExpansion(t *testing.T) {
	p, err := build(t, "FROM xo://t\nARG A=arg\nENV A=env\nOUTPUT --as=template $A\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Stage.Outputs[0].Name; got != "env" {
		t.Errorf("got %q", got)
	}
}

func TestUnsetArgIsNotSet(t *testing.T) {
	p, err := build(t, "FROM xo://t\nARG A\nOUTPUT --as=template\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if a := p.Stage.Steps[0].(*instructions.Arg); a.Set {
		t.Errorf("ARG A = %+v, want unset", a)
	}
}

func TestWarnings(t *testing.T) {
	p, err := build(t, "FROM xo://t\n", map[string]string{"NOPE": "1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"no OUTPUT: the VM will be destroyed at the end of the build",
		"--build-arg NOPE is not declared by any ARG",
	}
	if !reflect.DeepEqual(p.Warnings, want) {
		t.Errorf("warnings = %q", p.Warnings)
	}
}

func TestErrors(t *testing.T) {
	tests := []struct {
		src string
		err string
	}{
		{"ARG A", "Dangofile:1: no FROM instruction"},
		{"RUN x\nFROM xo://t", "Dangofile:1: RUN before FROM"},
		{"FROM xo://t\nFROM xo://u", "Dangofile:2: multiple stages are not supported yet"},
		{"FROM qemu://t", `Dangofile:1: unknown builder "qemu"`},
		{"FROM --ram=1G xo://t", "Dangofile:1: xo: unknown flag --ram"},
		{"FROM xo://t\nRUN x", "Dangofile:2: RUN needs a connection to the VM"},
		{"FROM xo://t\nCOPY a b", "Dangofile:2: COPY needs a connection"},
		{"FROM xo://t\nWORKDIR /a", "Dangofile:2: WORKDIR needs a connection"},
		{"FROM xo://t\nREBOOT", "Dangofile:2: REBOOT needs a connection"},
		{"FROM xo://t\nENV --persist A=1", "Dangofile:2: ENV --persist needs a connection"},
		{"FROM xo://t\nCONNECT ssh --user=a\nCONNECT ssh --user=b", "Dangofile:3: CONNECT already set at line 2"},
		{"FROM xo://t\nOUTPUT --as=template\nOUTPUT --as=template", "Dangofile:3: only one OUTPUT --as=template"},
		{"FROM xo://t\nOUTPUT --format=xva a\nOUTPUT --format=xva a", "Dangofile:3: OUTPUT a already written at line 2"},
		{"FROM xo://t\nEXPOSE 80", "Dangofile:2: EXPOSE has no meaning for a VM image"},
		{"FROM xo://t\nCMD x", "Dangofile:2: CMD has no meaning"},
		{"FROM xo://t\nLABEL a=b", "Dangofile:2: LABEL is not supported yet"},
		{"FROM xo://t\nADD a b", "Dangofile:2: ADD is not supported yet"},
		{"FROM xo://t\nBOOT <enter>", "Dangofile:2: BOOT is not supported yet"},
		{"FROM xo://t\nFOO", "Dangofile:2: unknown instruction FOO"},
		{"FROM xo://t\nUSER a:b", "Dangofile:2: USER: groups are not supported"},
	}
	for _, tt := range tests {
		_, err := build(t, tt.src, nil)
		if err == nil || !strings.HasPrefix(err.Error(), tt.err) {
			t.Errorf("%q: got error %v, want prefix %q", tt.src, err, tt.err)
		}
	}
}
