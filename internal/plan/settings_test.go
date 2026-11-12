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

// TestParentSettings checks dango.parent: a stage that starts from a stage
// of a plugin that is not in the table gets the section, then
// dango.parent (with the markers of the parent's artifact), then its FROM
// flags; the variables are reserved.
func TestParentSettings(t *testing.T) {
	settings, err := LoadSettings(writeSettings(t, `
libvirt:
  dango:
    parent:
      iso_url: ${DANGO_PARENT_FILE}
      iso_checksum: none
      tags: ['${DANGO_PARENT_ID}', '${REGION}']
  iso_url: https://example.org/${REGION}.qcow2
  iso_checksum: file:https://example.org/SHA256SUMS
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
	p, err := build("ARG REGION=eu\nFROM plugin://libvirt AS base\nFROM --memory=4096 --iso_checksum=sha256:abc base AS app\n")
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []map[string]string{
		{"iso_url": "https://example.org/eu.qcow2", "iso_checksum": "file:https://example.org/SHA256SUMS", "memory": "2048"},
		{"iso_url": ParentFileMarker, "iso_checksum": "sha256:abc", "memory": "4096", "tags": `["` + ParentIDMarker + `","eu"]`},
	} {
		got, err := p.Stages[i].PackerFlags()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("stage %d: %q, %v; want %q", i, got, err, want)
		}
	}

	for _, tt := range []struct{ src, err string }{
		{"ARG DANGO_PARENT_ID=x\nFROM plugin://libvirt\n", "DANGO_PARENT_ID is reserved: dango.parent in dango.yaml sets it"},
		{"FROM plugin://libvirt\nENV DANGO_PARENT_FILE=x\n", "DANGO_PARENT_FILE is reserved"},
		{"FROM plugin://other AS a\nFROM a\n", "opendango does not know how plugin://other starts from the artifact of a stage: set dango.parent in the other section"},
	} {
		if _, err := build(tt.src); err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("%q: err = %v, want %q", tt.src, err, tt.err)
		}
	}
	if _, err := LoadSettings(writeSettings(t, "libvirt:\n  dango:\n    parent:\n      ssh_password: x\n")); err == nil || !strings.Contains(err.Error(), "parent.ssh_password: no secret") {
		t.Errorf("secret in parent: %v", err)
	}
}

// TestExpandParent checks that dango.parent is expanded at build time with
// the artifact, so that shell patterns apply to it (an AMI id without its
// region), and that ${DANGO_PARENT_FILE} needs a file.
func TestExpandParent(t *testing.T) {
	settings, err := LoadSettings(writeSettings(t, `
amazon-ebs:
  dango:
    parent:
      source_ami: ${DANGO_PARENT_ID#*:}
      ami_description: from ${DANGO_PARENT_ID} in ${REGION}
  region: eu-west-3
libvirt:
  dango:
    parent:
      iso_url: ${DANGO_PARENT_FILE}
`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := parser.Parse(strings.NewReader("ARG REGION=eu-west-3\nFROM plugin://amazon-ebs AS base\nFROM base AS app\nFROM plugin://libvirt AS q\nFROM q AS q2\n"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := BuildSettings("Dangofile", res, nil, settings)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Stages[1].ExpandParent("eu-west-3:ami-0abc", "")
	if want := map[string]string{"source_ami": "ami-0abc", "ami_description": "from eu-west-3:ami-0abc in eu-west-3"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("amazon-ebs: %v, %v; want %v", got, err, want)
	}
	if _, err := p.Stages[3].ExpandParent("id", ""); err == nil || !strings.Contains(err.Error(), "libvirt.dango.parent.iso_url: ${DANGO_PARENT_FILE}: the artifact of stage q has no file") {
		t.Errorf("libvirt without a file: %v", err)
	}
	if got, err := p.Stages[3].ExpandParent("id", "/out/q.qcow2"); err != nil || got["iso_url"] != "/out/q.qcow2" {
		t.Errorf("libvirt: %v, %v", got, err)
	}
}
