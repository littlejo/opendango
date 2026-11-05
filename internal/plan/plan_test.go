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
	want := []string{"Arg", "Arg", "User", "Env", "Env", "Workdir", "Run", "Run", "Copy", "Reboot", "Breakpoint"}
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("steps = %v, want %v", kinds, want)
	}
	if bp := s.Steps[len(s.Steps)-1].(*instructions.Breakpoint); bp.Message != "check debian 13" {
		t.Errorf("BREAKPOINT message = %q", bp.Message)
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
		{"FROM xo://t\nBREAKPOINT", "Dangofile:2: BREAKPOINT needs a connection"},
		{"FROM plugin://Bad Name", "Dangofile:1: FROM requires exactly one source"},
		{"FROM plugin://xenserver_ISO", "Dangofile:1: plugin: invalid source"},
		{"FROM --ssh_username=root plugin://xenserver-iso", "Dangofile:1: plugin: --ssh_username is set by CONNECT ssh --user=<user>"},
		{"FROM --network_names=[x plugin://xenserver-iso", "Dangofile:1: plugin: --network_names: invalid JSON"},
		{"FROM plugin://xenserver-iso\nOUTPUT --as=template", "Dangofile:2: OUTPUT --as=template has no meaning in a plugin://xenserver-iso stage"},
		{"FROM plugin://xenserver-iso AS p\nFROM p", "Dangofile:2: FROM p: opendango does not know how plugin://xenserver-iso starts from the artifact of a stage: set dango.parent in the xenserver-iso section of dango.yaml"},
		{"FROM plugin://xenorchestra AS p\nFROM --template=t p", "Dangofile:2: FROM p: plugin: --template cannot be set when starting from another stage"},
		{"FROM plugin://xenorchestra AS p\nFROM --iso=x p", "Dangofile:2: FROM p: plugin: --iso cannot be set when starting from another stage"},
		{"FROM plugin://xenorchestra AS p\nFROM --tags=[] p", "Dangofile:2: plugin: --tags is set by LABEL"},
		{"FROM xo://t\nCONNECT ssh --user=u\nBREAKPOINT --x", "Dangofile:3: BREAKPOINT: unknown flag --x"},
		{"FROM xo://t\nENV --persist A=1", "Dangofile:2: ENV --persist needs a connection"},
		{"FROM xo://t\nCONNECT ssh --user=a\nCONNECT ssh --user=b", "Dangofile:3: CONNECT already set at line 2"},
		{"FROM xo://t\nOUTPUT --as=template\nOUTPUT --as=template", "Dangofile:3: only one OUTPUT --as=template"},
		{"FROM xo://t\nOUTPUT --format=xva a\nOUTPUT --format=xva a", "Dangofile:3: OUTPUT a already written at line 2"},
		{"FROM xo://t\nEXPOSE 80", "Dangofile:2: EXPOSE has no meaning for a VM image"},
		{"FROM xo://t\nCMD x", "Dangofile:2: CMD has no meaning"},
		{"FROM xo://t\nLABEL a=b\nLABEL c=d\nOUTPUT --format=xva x.xva", "Dangofile:2: LABEL needs OUTPUT --as=template"},
		{"FROM xo://t AS a\nLABEL a=b\nFROM a\nOUTPUT --as=template", "Dangofile:2: LABEL needs OUTPUT --as=template"},
		{"FROM xo://t\nLABEL opendango-cache:x=1\nOUTPUT --as=template", "Dangofile:2: LABEL opendango-cache:x: keys starting with opendango-cache are reserved"},
		{"FROM plugin://p-b\nLABEL a=b", "Dangofile:2: LABEL is not supported in a plugin://p-b stage"},
		// plugin://xenorchestra is in the table: OUTPUT and LABEL map to
		// its settings, which are not FROM flags.
		{"FROM plugin://xenorchestra\nLABEL a=b", "Dangofile:2: LABEL needs OUTPUT --as=template"},
		{"FROM --template_name=x plugin://xenorchestra", "Dangofile:1: plugin: --template_name is set by OUTPUT --as=template <name>"},
		{"FROM --tags=[] plugin://xenorchestra", "Dangofile:1: plugin: --tags is set by LABEL"},
		{"FROM xo://t\nADD a b", "Dangofile:2: ADD needs a connection"},
		{"FROM xo://t\nFOO", "Dangofile:2: unknown instruction FOO"},
		{"FROM xo://t\nUSER a:b", "Dangofile:2: USER: groups are not supported"},
		{"FROM xo://t\nSERVE http\nSERVE www", "Dangofile:3: SERVE already set at line 2"},
		{"FROM xo://t\nCONNECT ssh --user=u\nSERVE http", "Dangofile:3: SERVE must come before BOOT, CONNECT"},
		{"FROM xo://t AS a\nCONNECT ssh --user=u\nFROM a\nRUN x\nSERVE http", "Dangofile:5: SERVE must come before BOOT, CONNECT"},
		{"FROM xo://t\nCONNECT ssh --user=u\nFROM xo://t\nENV --persist A=1\nSERVE http", "Dangofile:4: ENV --persist needs a connection"},
		{"FROM xo://t\nSERVE ../http", "Dangofile:2: SERVE: \"../http\" is outside the Dangofile directory"},
		{"FROM --remote_host=h plugin://xenserver-iso\nSERVE --addr=10.0.0.1 http", "Dangofile:2: SERVE --addr is not supported in a plugin://xenserver-iso stage"},
		{"FROM xo://t\nSERVE http\nENV URL=http://${DANGO_HTTP_ADDR}/ks.cfg", "Dangofile:3: ENV URL: ${DANGO_HTTP_ADDR} is only set after SERVE"},
		{"FROM xo://t\nBOOT <enter>\nSERVE http", "Dangofile:3: SERVE must come before BOOT"},
		{"FROM xo://t\nCONNECT ssh --user=u\nBOOT <enter>", "Dangofile:3: BOOT must come before CONNECT"},
		{"FROM xo://t\nBOOT url=http://${DANGO_HTTP_ADDR}/ks.cfg", "Dangofile:2: BOOT: ${DANGO_HTTP_ADDR} is only set after SERVE"},
		{"FROM xo://t\nBOOT <wait0s>", "Dangofile:2: BOOT: Expecting a positive wait value"},
		{"FROM xo://t\nBOOT", "Dangofile:2: BOOT requires the keys to type"},
		{"FROM xo://t\nSERVE http\nENV DANGO_HTTP_ADDR=x", "Dangofile:3: DANGO_HTTP_ADDR is reserved"},
		{"ARG DANGO_HTTP_ADDR\nFROM xo://t", "Dangofile:1: DANGO_HTTP_ADDR is reserved"},
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

func TestPackerStage(t *testing.T) {
	src := "ARG HOST=10.0.0.1\nFROM --remote_host=${HOST} --network_names='[\"LAN\"]' plugin://xenserver-iso AS golden\nCONNECT ssh --user=debian\nRUN true\n"
	p, err := build(t, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := p.Stages[0]
	if !s.IsPacker() || s.From.Source != "xenserver-iso" || s.From.Flags["remote_host"] != "10.0.0.1" || s.From.Flags["network_names"] != `["LAN"]` {
		t.Errorf("stage = %+v", s.From)
	}
	// The plugin produces the artifact: no warning about a missing OUTPUT.
	if len(p.Warnings) != 0 {
		t.Errorf("warnings = %v", p.Warnings)
	}
}

// TestPackerKnownBuilder checks the stages of a builder of the table
// (plugin://xenorchestra): OUTPUT and LABEL are accepted, and a stage
// without OUTPUT gets the warning of the native stages.
func TestPackerKnownBuilder(t *testing.T) {
	p, err := build(t, "FROM plugin://xenorchestra AS golden\nLABEL team=web\nOUTPUT --format=xva --compress=zstd golden.xva\nOUTPUT --as=template golden\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := p.Stages[0]; len(s.Outputs) != 2 || len(s.Labels) != 1 || len(p.Warnings) != 0 {
		t.Errorf("outputs %v, labels %v, warnings %v", s.Outputs, s.Labels, p.Warnings)
	}
	p, err = build(t, "FROM plugin://xenorchestra AS golden\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "stage golden has no OUTPUT and no stage starts from it: the plugin's artifact will be deleted at the end of the stage"; len(p.Warnings) != 1 || p.Warnings[0] != want {
		t.Errorf("warnings = %q", p.Warnings)
	}
}

// TestPackerStageFlags checks the flags of a stage that starts from a
// plugin:// stage: its own, plus the inherited ones of its ancestors.
func TestPackerStageFlags(t *testing.T) {
	src := "FROM --endpoint=https://xo --insecure=true --template=t --pool=p --cpus=2 plugin://xenorchestra AS a\nFROM --cpus=4 a AS b\nFROM --insecure=false b AS c\n"
	p, err := build(t, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []map[string]string{
		{"endpoint": "https://xo", "insecure": "true", "template": "t", "pool": "p", "cpus": "2"},
		{"endpoint": "https://xo", "insecure": "true", "cpus": "4"},
		{"endpoint": "https://xo", "insecure": "false"},
	} {
		got, err := p.Stages[i].PackerFlags()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("stage %s: flags %v, %v; want %v", p.Stages[i].Name(), got, err, want)
		}
	}
	// a and b are parents: no warning about a missing OUTPUT for them.
	if want := []string{"stage c has no OUTPUT and no stage starts from it: the plugin's artifact will be deleted at the end of the stage"}; !reflect.DeepEqual(p.Warnings, want) {
		t.Errorf("warnings = %q", p.Warnings)
	}
}

// TestCopyFrom checks COPY --from: an earlier stage, the paths it fetches,
// the stages that wait for it, --target and the warnings.
func TestCopyFrom(t *testing.T) {
	src := `FROM xo://t AS build
CONNECT ssh --user=u
RUN true

FROM xo://t AS other
CONNECT ssh --user=u
RUN true

FROM xo://t AS app
CONNECT ssh --user=u
COPY --from=build /opt/app /usr/local/bin/
COPY --from=build /etc/app.conf /opt/app /etc/
OUTPUT --as=template app
`
	p, err := build(t, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, app := p.Stages[0], p.Stages[2]
	if !reflect.DeepEqual(b.Fetch, []string{"/etc/app.conf", "/opt/app"}) || len(app.CopyFrom) != 1 || app.CopyFrom[0] != b {
		t.Errorf("fetch %q, copy from %v", b.Fetch, app.CopyFrom)
	}
	// build feeds app: no warning about its missing OUTPUT; other has one.
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "stage other has no OUTPUT") {
		t.Errorf("warnings = %q", p.Warnings)
	}
	got, err := p.Select("app")
	if err != nil || len(got) != 2 || got[0] != b || got[1] != app {
		t.Errorf("Select(app) = %v, %v", got, err)
	}

	for _, tt := range []struct{ src, err string }{
		{"FROM xo://t AS a\nCONNECT ssh --user=u\nCOPY --from=b /x /y\nFROM xo://t AS b\n", `COPY --from=b: no earlier stage named "b"`},
		{"FROM xo://t AS a\nCONNECT ssh --user=u\nCOPY --from=a /x /y\n", "a stage cannot copy from itself"},
		{"FROM xo://t AS a\nCONNECT ssh --user=u\nCOPY --from=debian:13 /x /y\n", "images are not supported"},
	} {
		if _, err := build(t, tt.src, nil); err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("%q: err = %v, want %q", tt.src, err, tt.err)
		}
	}
}

// TestPostProcessors checks the OUTPUT post-processors: allowed in any
// stage (a plugin that is not in the table too), one path per output, and
// a plugin's post-processor gets its dango.yaml section under its flags.
func TestPostProcessors(t *testing.T) {
	p, err := build(t, "FROM plugin://qemu AS q\nOUTPUT --format=manifest m.json\nOUTPUT --format=checksum SUMS\nOUTPUT --format=compress a.zip\nOUTPUT --output=b.box plugin://vagrant\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(p.Stages[0].Outputs); n != 4 || len(p.Warnings) != 0 {
		t.Errorf("outputs %d, warnings %q", n, p.Warnings)
	}
	if _, err := build(t, "FROM xo://t\nOUTPUT --format=checksum a.zip\nOUTPUT --format=compress a.zip\n", nil); err == nil || !strings.Contains(err.Error(), "OUTPUT a.zip already written at line 2") {
		t.Errorf("same path: %v", err)
	}

	settings, err := LoadSettings(writeSettings(t, "vagrant:\n  compression_level: 9\n  output: dist/x.box\n"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := parser.Parse(strings.NewReader("FROM xo://t\nOUTPUT --output=dist/${NAME}.box plugin://vagrant\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p, err = BuildSettings("Dangofile", res, map[string]string{}, settings); err != nil {
		t.Fatal(err)
	}
	if got := p.Stages[0].Outputs[0].Settings; !reflect.DeepEqual(got, map[string]string{"compression_level": "9", "output": "dist/.box"}) {
		t.Errorf("settings %v", got)
	}
}

// TestProvision checks the planning of PROVISION: a step that needs the
// connection, with its dango.yaml section under its flags.
func TestProvision(t *testing.T) {
	settings, err := LoadSettings(writeSettings(t, "ansible:\n  user: root\n  playbook_file: default.yml\n"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := parser.Parse(strings.NewReader("FROM xo://t\nCONNECT ssh --user=u\nPROVISION --playbook_file=site.yml plugin://ansible\n"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := BuildSettings("Dangofile", res, nil, settings)
	if err != nil {
		t.Fatal(err)
	}
	pr, ok := p.Stages[0].Steps[0].(*instructions.Provision)
	if !ok || !reflect.DeepEqual(pr.Settings, map[string]string{"user": "root", "playbook_file": "site.yml"}) {
		t.Errorf("steps %+v", p.Stages[0].Steps)
	}
	if _, err := build(t, "FROM plugin://qemu\nPROVISION plugin://ansible\n", nil); err == nil || !strings.Contains(err.Error(), "PROVISION needs a connection") {
		t.Errorf("no connection: %v", err)
	}
}

func TestServe(t *testing.T) {
	src := "ARG DIR=http\nFROM xo://t AS a\nARG DIR\nUSER root\nSERVE --port=8080 ${DIR}\nCONNECT ssh --user=u\nRUN curl http://$DANGO_HTTP_ADDR/ks.cfg\n" +
		"FROM a AS b\nOUTPUT --as=template\n"
	p, err := build(t, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sv := p.Stages[0].Serve; sv == nil || sv.Dir != "http" || sv.Port != 8080 || sv.Line() != 5 {
		t.Errorf("serve = %+v", sv)
	}
	// One server per stage: not inherited.
	if p.Stages[1].Serve != nil {
		t.Errorf("stage b inherits SERVE")
	}
}

func TestLabel(t *testing.T) {
	src := `ARG V=1
FROM xo://t AS base
ARG V
LABEL vendor=acme team=infra
OUTPUT --as=template base
LABEL version=$V

FROM base AS app
ENV T=web
LABEL "team"="${T}" app=shop
OUTPUT --as=template app

FROM base AS plain
OUTPUT --as=template plain
`
	p, err := build(t, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The child overrides in place and adds after; a stage without LABEL
	// inherits its parent's.
	want := map[string]string{
		"base":  "vendor=acme team=infra version=1",
		"app":   "vendor=acme team=web version=1 app=shop",
		"plain": "vendor=acme team=infra version=1",
	}
	for _, s := range p.Stages {
		var got []string
		for _, l := range s.Labels {
			got = append(got, l.Key+"="+l.Value)
		}
		if strings.Join(got, " ") != want[s.Name()] {
			t.Errorf("%s labels = %q, want %q", s.Name(), got, want[s.Name()])
		}
	}
}

func TestShell(t *testing.T) {
	// SHELL needs no connection: it only changes the following RUN.
	src := "FROM xo://t\nSHELL [\"/bin/bash\", \"-c\"]\nCONNECT ssh --user=u\nRUN echo\n"
	p, err := build(t, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sh, ok := p.Stages[0].Steps[0].(*instructions.Shell); !ok || sh.Line() != 2 || len(sh.Args) != 2 {
		t.Errorf("steps = %+v", p.Stages[0].Steps)
	}
	if _, err := build(t, "FROM xo://t\nSHELL bash\n", nil); err == nil || err.Error() != `Dangofile:2: SHELL requires the JSON form: SHELL ["executable", "parameters"]` {
		t.Errorf("err = %v", err)
	}
}

func TestBoot(t *testing.T) {
	src := "ARG HOST=web\nFROM xo://t\nARG HOST\nSERVE http\nBOOT <esc><wait> auto url=http://${DANGO_HTTP_ADDR}/p.cfg hostname=$HOST it's \"quoted\" \\$HOME\nBOOT <wait5>\nCONNECT ssh --user=u\n"
	p, err := build(t, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	boot := p.Stages[0].Boot
	want := `<esc><wait> auto url=http://` + instructions.ServeAddrMarker + `/p.cfg hostname=web it's "quoted" $HOME`
	if len(boot) != 2 || boot[0].Keys != want || boot[0].Line() != 5 || boot[1].Keys != "<wait5>" {
		t.Errorf("boot = %+v, want first %q", boot, want)
	}
}

// TestFromFlagQuotes checks that the quotes left in FROM flag values (JSON)
// survive the expansion, whatever the escape character.
func TestFromFlagQuotes(t *testing.T) {
	const from = `ARG N=LAN
FROM --network_names='["${N}", "it'"'"'s"]' --vm_name="Debian 13" plugin://xenserver-iso
`
	for _, src := range []string{from, "# escape=`\n" + from} {
		p, err := build(t, src, nil)
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		flags := p.Stages[0].From.Flags
		if flags["network_names"] != `["LAN", "it's"]` || flags["vm_name"] != "Debian 13" {
			t.Errorf("%q: flags = %q", src, flags)
		}
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
