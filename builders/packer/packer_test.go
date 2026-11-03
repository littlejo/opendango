package packer

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestParseConfig(t *testing.T) {
	cfg, err := ParseConfig("xenserver-iso", map[string]string{
		"vm_name":         "golden",
		"vm_memory":       "2048",
		"network_names":   `["LAN", "Admin"]`,
		"vm_tags":         `{"a": "b"}`,
		"remote_password": "s3cret",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"vm_name":         "golden",
		"vm_memory":       "2048",
		"network_names":   []any{"LAN", "Admin"},
		"vm_tags":         map[string]any{"a": "b"},
		"remote_password": "s3cret",
	}
	if cfg.Component != "xenserver-iso" || !reflect.DeepEqual(cfg.Settings, want) || !reflect.DeepEqual(cfg.Secrets, []string{"s3cret"}) {
		t.Errorf("got %+v", cfg)
	}

	for _, tt := range []struct {
		source string
		flags  map[string]string
		err    string
	}{
		{"Xen Server", nil, "invalid source"},
		{"xenserver-iso", map[string]string{"ssh_username": "x"}, "--ssh_username is set by CONNECT ssh --user=<user>"},
		{"xenserver-iso", map[string]string{"communicator": "winrm"}, "--communicator is set by CONNECT ssh"},
		{"xenserver-iso", map[string]string{"packer_force": "true"}, "reserved"},
		{"xenserver-iso", map[string]string{"http_directory": "http"}, "--http_directory is set by SERVE <dir>"},
		{"xenserver-iso", map[string]string{"http_port_min": "8000"}, "--http_port_min is set by SERVE --port=<port>"},
		{"xenserver-iso", map[string]string{"network_names": `["a"`}, "--network_names: invalid JSON"},
		{"xenserver-iso", map[string]string{"boot_command": `["a"]`}, "--boot_command is set by BOOT <keys>"},
	} {
		if _, err := ParseConfig(tt.source, tt.flags); err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("%s %v: err = %v, want %q", tt.source, tt.flags, err, tt.err)
		}
	}
}

func TestDiscoverAndFind(t *testing.T) {
	arch := "_x5.0_" + runtime.GOOS + "_" + runtime.GOARCH
	installed := t.TempDir()
	path := t.TempDir()
	files := map[string]bool{
		// The layout of `packer plugins install`.
		filepath.Join(installed, "github.com/vatesfr/xenserver/packer-plugin-xenserver_v0.9.0"+arch):               true,
		filepath.Join(installed, "github.com/vatesfr/xenserver/packer-plugin-xenserver_v0.11.4"+arch):              true,
		filepath.Join(installed, "github.com/vatesfr/xenserver/packer-plugin-xenserver_v0.11.4"+arch+"_SHA256SUM"): false,
		filepath.Join(installed, "github.com/other/xenserver/packer-plugin-xenserver_v9.0.0_x5.0_plan9_mips"):      true,
		filepath.Join(installed, "github.com/hashicorp/qemu/packer-plugin-qemu_v1.0.0"+arch):                       true,
		filepath.Join(path, "packer-plugin-xenserver-extra"):                                                       true,
		filepath.Join(path, "packer-plugin-notexec"):                                                               false,
		filepath.Join(path, "unrelated"):                                                                           true,
	}
	for f, exe := range files {
		os.MkdirAll(filepath.Dir(f), 0o755)
		mode := os.FileMode(0o644)
		if exe {
			mode = 0o755
		}
		if err := os.WriteFile(f, nil, mode); err != nil {
			t.Fatal(err)
		}
	}
	plugins := Discover([]string{installed, filepath.Join(installed, "missing")}, []string{path})
	var names []string
	for _, p := range plugins {
		names = append(names, p.Name+"@"+p.Version)
	}
	sort.Strings(names)
	if want := []string{"qemu@1.0.0", "xenserver-extra@", "xenserver@0.11.4", "xenserver@0.9.0"}; !reflect.DeepEqual(names, want) {
		t.Errorf("discovered %v, want %v", names, want)
	}

	for _, tt := range []struct {
		source, plugin, version, builder, err string
	}{
		{"xenserver-iso", "xenserver", "0.11.4", "iso", ""},
		{"xenserver-xva_iso", "xenserver", "0.11.4", "xva_iso", ""},
		// The longest plugin name wins.
		{"xenserver-extra-iso", "xenserver-extra", "", "iso", ""},
		{"qemu", "qemu", "1.0.0", defaultBuilder, ""},
		{"vsphere-iso", "", "", "", "no plugin for \"vsphere-iso\""},
	} {
		pl, builder, err := Find(plugins, tt.source)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%s: err = %v", tt.source, err)
			}
			continue
		}
		if err != nil || pl.Name != tt.plugin || pl.Version != tt.version || builder != tt.builder {
			t.Errorf("%s: got %+v %q %v", tt.source, pl, builder, err)
		}
	}
}

var (
	testPluginOnce sync.Once
	testPluginPath string
	testPluginErr  error
)

// testPlugin builds packer-plugin-opendangotest once and returns its
// path.
func testPlugin(t *testing.T) string {
	t.Helper()
	testPluginOnce.Do(func() {
		dir, err := os.MkdirTemp("", "opendango-plugin-")
		if err != nil {
			testPluginErr = err
			return
		}
		testPluginPath = filepath.Join(dir, "packer-plugin-opendangotest")
		out, err := exec.Command("go", "build", "-o", testPluginPath, "./testdata/packer-plugin-opendangotest").CombinedOutput()
		if err != nil {
			testPluginErr = errors.New(string(out))
		}
	})
	if testPluginErr != nil {
		t.Fatalf("building the test plugin: %v", testPluginErr)
	}
	return testPluginPath
}

func TestBuild(t *testing.T) {
	pl := &Plugin{Name: "opendangotest", Path: testPlugin(t)}
	d, err := pl.Describe(context.Background())
	if err != nil || !reflect.DeepEqual(d.Builders, []string{"local"}) {
		t.Fatalf("describe: %+v, %v", d, err)
	}

	root := t.TempDir()
	src := filepath.Join(t.TempDir(), "src")
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "sub", "f"), []byte("dir file"), 0o600)
	os.WriteFile(filepath.Join(src, "single"), []byte("single file"), 0o640)

	var mu sync.Mutex
	var said []string
	ui := &Ui{Print: func(l string) { mu.Lock(); said = append(said, l); mu.Unlock() }, Secrets: []string{"s3cret"}}
	b := &Build{
		Plugin:  pl,
		Builder: "local",
		Configs: []map[string]any{
			{"root": root, "count": "3", "items": []any{"a", "b"}, "password": "s3cret"},
			{"packer_build_name": "golden", "packer_builder_type": "opendangotest-local", "packer_on_error": "cleanup"},
		},
		Ui: ui,
		Provision: func(ctx context.Context, c *Comm) error {
			var out strings.Builder
			if err := c.Run(ctx, "echo s3cret; pwd", &out, &out); err != nil {
				return err
			}
			ui.Say("pwd: " + out.String())
			if err := c.Upload(ctx, filepath.Join(src, "single"), filepath.Join(root, "new", "single")); err != nil {
				return err
			}
			if err := c.Upload(ctx, src, filepath.Join(root, "tree")); err != nil {
				return err
			}
			var exitErr *ExitError
			if err := c.Run(ctx, "exit 4", &out, &out); !errors.As(err, &exitErr) || exitErr.Code != 4 {
				t.Errorf("exit 4: %v", err)
			}
			return nil
		},
	}
	artifact, err := b.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if artifact != "local machine at "+root {
		t.Errorf("artifact = %q", artifact)
	}
	log := strings.Join(said, "\n")
	for _, want := range []string{
		"Warning: this is a test plugin",
		// Strings become the plugin's types; JSON lists stay lists.
		"config: build=golden type=opendangotest-local on_error=cleanup count=3 items=a,b",
		"pwd: <sensitive>\n" + root,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	for p, want := range map[string]string{"new/single": "single file", "tree/sub/f": "dir file"} {
		if got, err := os.ReadFile(filepath.Join(root, p)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", p, got, err)
		}
	}
	if info, err := os.Stat(filepath.Join(root, "new", "single")); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("mode of the uploaded file: %v, %v", info, err)
	}
}

func TestBuildErrors(t *testing.T) {
	pl := &Plugin{Name: "opendangotest", Path: testPlugin(t)}
	ui := &Ui{Print: func(string) {}}

	// Prepare fails: no root.
	_, err := (&Build{Plugin: pl, Builder: "local", Configs: []map[string]any{{}}, Ui: ui}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid configuration") || !strings.Contains(err.Error(), "root is required") {
		t.Errorf("Prepare: err = %v", err)
	}

	// The provision error comes back as is.
	mine := errors.New("Dangofile:3: RUN: exit code 1")
	_, err = (&Build{Plugin: pl, Builder: "local", Configs: []map[string]any{{"root": t.TempDir()}}, Ui: ui,
		Provision: func(context.Context, *Comm) error { return mine }}).Run(context.Background())
	if err != mine {
		t.Errorf("provision: err = %v", err)
	}

	// The builder fails after provisioning.
	_, err = (&Build{Plugin: pl, Builder: "local", Configs: []map[string]any{{"root": t.TempDir(), "fail": "true"}}, Ui: ui,
		Provision: func(context.Context, *Comm) error { return nil }}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "failing as asked") {
		t.Errorf("run: err = %v", err)
	}
}
