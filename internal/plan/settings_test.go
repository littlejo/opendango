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
