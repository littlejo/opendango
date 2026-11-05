package plan

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/littlejo/opendango/internal/parser"
)

func writeSettings(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), SettingsFile)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadSettings(t *testing.T) {
	s, err := LoadSettings(writeSettings(t, `
qemu:
  disk_size: 10G
  memory: 2048
  headless: true
  ratio: 1.5
  cd_files: [cidata/user-data, "it's"]
  cd_content: {meta-data: "instance-id: x"}
xenorchestra:
`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]string{
		"qemu": {
			"disk_size":  "10G",
			"memory":     "2048",
			"headless":   "true",
			"ratio":      "1.5",
			"cd_files":   `["cidata/user-data","it's"]`,
			"cd_content": `{"meta-data":"instance-id: x"}`,
		},
		"xenorchestra": {},
	}
	if !reflect.DeepEqual(s.Sections, want) {
		t.Errorf("sections = %v\nwant %v", s.Sections, want)
	}

	for _, tt := range []struct{ content, err string }{
		{"qemu:\n  ssh_password: x\n", "qemu.ssh_password: no secret in dango.yaml: use ARG and --build-arg"},
		{"qemu:\n  ssh_username: debian\n", "qemu: plugin: --ssh_username is set by CONNECT ssh --user=<user>"},
		{"xenorchestra:\n  tags: [a]\n", "xenorchestra: plugin: --tags is set by LABEL"},
		{"Bad_Source:\n  a: b\n", `Bad_Source: plugin: invalid source "Bad_Source"`},
		{"qemu:\n  disk_size:\n", "qemu.disk_size: no value"},
		{"qemu: [a]\n", "cannot unmarshal"},
	} {
		if _, err := LoadSettings(writeSettings(t, tt.content)); err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("%q: err = %v, want %q", tt.content, err, tt.err)
		}
	}
}

// TestBuildSettings checks how dango.yaml reaches the stages: expanded with
// the global ARGs (quotes kept), the FROM flags first, the root-only
// settings left out of a stage that starts from another one, and a warning
// for an unused section.
func TestBuildSettings(t *testing.T) {
	settings, err := LoadSettings(writeSettings(t, `
xenorchestra:
  template: ${BASE}
  pool: p1
  network: '["LAN"]'
  cpus: 2
  endpoint: https://xo
qemu:
  memory: 2048
`))
	if err != nil {
		t.Fatal(err)
	}
	src := "ARG BASE=debian\nFROM --cpus=4 plugin://xenorchestra AS base\nFROM base AS app\n"
	res, err := parser.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	p, err := BuildSettings("Dangofile", res, nil, settings)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []map[string]string{
		{"template": "debian", "pool": "p1", "network": `["LAN"]`, "cpus": "4", "endpoint": "https://xo"},
		// No template nor pool: the parent is the source.
		{"network": `["LAN"]`, "cpus": "2", "endpoint": "https://xo"},
	} {
		got, err := p.Stages[i].PackerFlags()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("stage %d: %v, %v; want %v", i, got, err, want)
		}
	}
	if want := settings.File + ": no stage uses plugin://qemu"; !strings.Contains(strings.Join(p.Warnings, "\n"), want) {
		t.Errorf("warnings %q lack %q", p.Warnings, want)
	}
}

// TestFileConnect checks the dango.connect option: the CONNECT of the
// plugin:// stages without one (BOOT may still come after FROM), replaced by
// a CONNECT in the stage, inherited by the children, and the errors.
func TestFileConnect(t *testing.T) {
	settings, err := LoadSettings(writeSettings(t, `
qemu:
  dango:
    connect: ssh --user=${USER_NAME} --timeout=20m
  memory: 2048
`))
	if err != nil {
		t.Fatal(err)
	}
	build := func(src string) (*Plan, error) {
		res, err := parser.Parse(strings.NewReader(src))
		if err != nil {
			t.Fatal(err)
		}
		return BuildSettings("Dangofile", res, nil, settings)
	}
	p, err := build("ARG USER_NAME=debian\nFROM plugin://qemu AS a\nBOOT <enter>\nRUN true\n\nFROM plugin://qemu AS b\nCONNECT ssh --user=root\nRUN true\n")
	if err != nil {
		t.Fatal(err)
	}
	if c := p.Stages[0].Connect; c == nil || c.User != "debian" || c.Timeout.String() != "20m0s" {
		t.Errorf("stage a: connect %+v", c)
	}
	if c := p.Stages[1].Connect; c == nil || c.User != "root" {
		t.Errorf("stage b: connect %+v", c)
	}
	// The option is not a plugin setting.
	if flags, _ := p.Stages[0].PackerFlags(); flags["dango"] != "" || flags["memory"] != "2048" {
		t.Errorf("flags %v", flags)
	}
	// Without the option, RUN needs a CONNECT.
	if _, err := build("FROM plugin://other\nRUN true\n"); err == nil || !strings.Contains(err.Error(), "RUN needs a connection") {
		t.Errorf("no connect: %v", err)
	}

	for _, tt := range []struct{ content, err string }{
		{"qemu:\n  dango:\n    connect: ssh\n", "qemu.dango.connect: CONNECT requires --user=<user>"},
		{"qemu:\n  dango:\n    user: root\n", "qemu.dango: yaml: unmarshal errors"},
		{"qemu:\n  dango: x\n", "qemu.dango: yaml: unmarshal errors"},
	} {
		s, err := LoadSettings(writeSettings(t, tt.content))
		if err == nil {
			res, _ := parser.Parse(strings.NewReader("FROM plugin://qemu\n"))
			_, err = BuildSettings("Dangofile", res, nil, s)
		}
		if err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("%q: err = %v, want %q", tt.content, err, tt.err)
		}
	}
}
