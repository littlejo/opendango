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
	s := p.Stages[0]
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
	if got := p.Stages[0].From.Source; got != "t-global" {
		t.Errorf("FROM source = %q", got)
	}
	if got := p.Stages[0].Outputs[0].Name; got != "global--l-global" {
		t.Errorf("output name = %q", got)
	}
	args := p.Stages[0].Steps
	if a := args[0].(*instructions.Arg); a.Value != "global" || !a.Set {
		t.Errorf("stage ARG V = %+v", a)
	}

	p, err = build(t, src, map[string]string{"V": "cli", "LOCAL": "c"})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Stages[0].Outputs[0].Name; got != "cli--c" {
		t.Errorf("with --build-arg: output name = %q", got)
	}
}

func TestEnvOverridesArgInExpansion(t *testing.T) {
	p, err := build(t, "FROM xo://t\nARG A=arg\nENV A=env\nOUTPUT --as=template $A\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Stages[0].Outputs[0].Name; got != "env" {
		t.Errorf("got %q", got)
	}
}

func TestUnsetArgIsNotSet(t *testing.T) {
	p, err := build(t, "FROM xo://t\nARG A\nOUTPUT --as=template\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if a := p.Stages[0].Steps[0].(*instructions.Arg); a.Set {
		t.Errorf("ARG A = %+v, want unset", a)
	}
}

func TestWarnings(t *testing.T) {
	p, err := build(t, "FROM xo://t\n", map[string]string{"NOPE": "1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"stage stage-0 has no OUTPUT and no stage starts from it: its VM will be destroyed at the end of the build",
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
		{"FROM xo://t AS a\nFROM xo://u AS a", `Dangofile:2: stage "a" already defined at line 1`},
		{"FROM xo://t AS a\nFROM b", `Dangofile:2: FROM b: no earlier stage named "b"`},
		{"FROM a\nFROM xo://t AS a", `Dangofile:1: FROM a: no earlier stage named "a"`},
		{"FROM xo://t AS a\nFROM --template=x a", "Dangofile:2: xo: --template cannot be set when starting from another stage"},
		{"FROM xo://t AS a\nFROM --pool=x a", "Dangofile:2: xo: --pool cannot be set when starting from another stage"},
		{"FROM xo://t AS a\nFROM a\nRUN x", "Dangofile:3: RUN needs a connection"},
		{"FROM xo://t AS a\nCONNECT ssh --user=u\nFROM a\nCONNECT ssh --user=v\nCONNECT ssh --user=w", "Dangofile:5: CONNECT already set at line 4"},
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

func TestMultiStage(t *testing.T) {
	src := `ARG BASE=debian
FROM --cpus=2 xo://${BASE}-13 AS golden
CONNECT ssh --user=debian
ARG LOCAL=only-golden
ENV APP=golden TIER=base
RUN true
OUTPUT --as=template golden

FROM --cpus=4 golden AS app
ENV APP=app
RUN true
OUTPUT --as=template "${APP}-${TIER}-${LOCAL}-${BASE}"

FROM golden AS other
CONNECT ssh --user=admin
OUTPUT --format=xva other.xva
`
	p, err := build(t, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Stages) != 3 {
		t.Fatalf("got %d stages", len(p.Stages))
	}
	golden, app, other := p.Stages[0], p.Stages[1], p.Stages[2]
	if app.Parent != golden || other.Parent != golden || golden.Parent != nil {
		t.Errorf("parents: app=%v other=%v golden=%v", app.Parent, other.Parent, golden.Parent)
	}
	if len(golden.Children) != 2 || app.Root() != golden {
		t.Errorf("children = %d, root = %v", len(golden.Children), app.Root())
	}
	if app.Connect != golden.Connect {
		t.Errorf("app does not inherit CONNECT: %+v", app.Connect)
	}
	if other.Connect.User != "admin" {
		t.Errorf("other CONNECT = %+v", other.Connect)
	}
	// ENV is inherited, ARG is not (Docker semantics); FROM sees global ARGs.
	if got := app.Outputs[0].Name; got != "app-base--" {
		t.Errorf("app template name = %q", got)
	}
	if app.From.Stage != "golden" || app.From.Flags["cpus"] != "4" {
		t.Errorf("app FROM = %+v", app.From)
	}
	if golden.TemplateOutput() == nil || other.TemplateOutput() != nil {
		t.Errorf("TemplateOutput: golden=%v other=%v", golden.TemplateOutput(), other.TemplateOutput())
	}
	if len(p.Warnings) != 0 {
		t.Errorf("warnings = %q", p.Warnings)
	}
}

func TestStageNames(t *testing.T) {
	p, err := build(t, "FROM xo://t\nFROM xo://u AS named\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Stages[0].Name() != "stage-0" || p.Stages[1].Name() != "named" {
		t.Errorf("names = %q, %q", p.Stages[0].Name(), p.Stages[1].Name())
	}
}

func TestSelect(t *testing.T) {
	p, err := build(t, "FROM xo://t AS a\nOUTPUT --as=template\nFROM a AS b\nOUTPUT --as=template\nFROM b AS c\nOUTPUT --as=template\nFROM xo://u AS d\nOUTPUT --as=template\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	names := func(stages []*Stage) string {
		var out []string
		for _, s := range stages {
			out = append(out, s.Name())
		}
		return strings.Join(out, ",")
	}
	for target, want := range map[string]string{"": "a,b,c,d", "c": "a,b,c", "a": "a", "d": "d"} {
		got, err := p.Select(target)
		if err != nil || names(got) != want {
			t.Errorf("Select(%q) = %s, %v; want %s", target, names(got), err, want)
		}
	}
	if _, err := p.Select("nope"); err == nil || !strings.Contains(err.Error(), `no stage named "nope" (stages: [a b c d])`) {
		t.Errorf("unknown target: %v", err)
	}
}

func TestUnusedStageWarning(t *testing.T) {
	p, err := build(t, "FROM xo://t AS base\nFROM base AS app\nOUTPUT --as=template\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("base is used by app, no warning expected: %q", p.Warnings)
	}
}
