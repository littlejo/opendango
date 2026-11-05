package build

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/littlejo/opendango/builders/packer"
	"github.com/littlejo/opendango/internal/parser"
	"github.com/littlejo/opendango/internal/plan"
)

// provisionerOf finds the test plugin's provisioners: opendangotest-<name>.
func provisionerOf(t *testing.T) FindPluginFunc {
	find := testPlugin(t)
	return func(ctx context.Context, source string) (*packer.Plugin, string, error) {
		pl, _, err := find(ctx, "opendangotest-local")
		return pl, strings.TrimPrefix(source, "opendangotest-"), err
	}
}

// TestProvision runs the test plugin's shell provisioner in a native stage
// (through the adapter of the native connection: commands, upload, download)
// and in a plugin:// stage (the plugin's communicator), with JSON settings
// and a dango.yaml section.
func TestProvision(t *testing.T) {
	for _, builder := range []string{"xo://t", "plugin://opendangotest-local"} {
		t.Run(builder, func(t *testing.T) {
			ctxDir, report, remote := t.TempDir(), filepath.Join(t.TempDir(), "report"), t.TempDir()
			yaml := filepath.Join(ctxDir, "dango.yaml")
			os.WriteFile(yaml, []byte("opendangotest-shell:\n  command: echo from-yaml\n"), 0o644)
			settings, err := plan.LoadSettings(yaml)
			if err != nil {
				t.Fatal(err)
			}
			from, upload := "FROM xo://t", " --upload_to="+remote
			if strings.HasPrefix(builder, "plugin://") {
				// The test plugin's communicator cannot download.
				from, upload = "FROM --root="+t.TempDir()+" "+builder, ""
			}
			src := fmt.Sprintf("%s\nCONNECT ssh --user=tester\nRUN true\nPROVISION --report=%s --args='[\"a b\", \"c\"]'%s plugin://opendangotest-shell\nRUN true\n", from, report, upload)
			res, err := parser.Parse(strings.NewReader(src))
			if err != nil {
				t.Fatal(err)
			}
			p, err := plan.BuildSettings("Dangofile", res, nil, settings)
			if err != nil {
				t.Fatal(err)
			}
			var log syncBuffer
			opts := Options{ContextDir: ctxDir, Out: &log, NoCache: true, NewBuilder: only(&fakeBuilder{}), Dial: dialTo(newLocalComm(t)), FindPlugin: testPlugin(t), FindProvisioner: provisionerOf(t)}
			if err := Run(context.Background(), p, opts); err != nil {
				t.Fatalf("%v\n%s", err, log.String())
			}
			got := readFile(t, report)
			for _, want := range []string{"stdin=through stdin\n", "output=from-yaml\n", "conn=ssh user=tester port=22 host=true key=true\n", "args=a b|c\n"} {
				if !strings.Contains(got, want) {
					t.Errorf("report lacks %q:\n%s", want, got)
				}
			}
			if upload != "" && !strings.Contains(got, "download=uploaded dir=uploaded\n") {
				t.Errorf("report:\n%s", got)
			}
			if !strings.Contains(log.String(), "PROVISION plugin://opendangotest-shell\n") {
				t.Errorf("log:\n%s", log.String())
			}
		})
	}
}

func TestProvisionFailure(t *testing.T) {
	src := fmt.Sprintf("FROM xo://t\nCONNECT ssh --user=tester\nPROVISION --report=%s --fail=true plugin://opendangotest-shell\n", filepath.Join(t.TempDir(), "r"))
	b := &fakeBuilder{}
	err := Run(context.Background(), mustPlan(t, src), Options{NoCache: true, NewBuilder: only(b), Dial: dialTo(newLocalComm(t)), FindProvisioner: provisionerOf(t)})
	if err == nil || !strings.Contains(err.Error(), "Dangofile:3: PROVISION: plugin: opendangotest provisioner shell: failing as asked") || !contains(b.calls, "destroy") {
		t.Errorf("err = %v, calls %q", err, b.calls)
	}
}

// TestProvisionCacheKey checks that the files a PROVISION setting names are
// part of the cache key.
func TestProvisionCacheKey(t *testing.T) {
	ctxDir := t.TempDir()
	os.WriteFile(filepath.Join(ctxDir, "site.yml"), []byte("- hosts: all\n"), 0o644)
	p := mustPlan(t, "FROM xo://t\nCONNECT ssh --user=u\nPROVISION --playbook_file=site.yml --extra_arguments='[\"-v\"]' plugin://ansible\nOUTPUT --as=template x\n")
	key := func() string {
		k, err := cacheKey(p.File, p.Stages[0], "", ctxDir)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	before := key()
	os.WriteFile(filepath.Join(ctxDir, "site.yml"), []byte("- hosts: all\n  become: true\n"), 0o644)
	if key() == before {
		t.Error("the playbook changed, not the key")
	}
}

// TestCheckProvisioners checks that validate has the provisioners check
// their settings (Prepare).
func TestCheckProvisioners(t *testing.T) {
	testPlugin(t)
	t.Setenv("PACKER_PLUGIN_PATH", filepath.Dir(pluginPath))
	for _, tt := range []struct{ step, err string }{
		{"PROVISION --report=r plugin://opendangotest-shell", ""},
		{"PROVISION --command=true plugin://opendangotest-shell", "Dangofile:3: plugin://opendangotest-shell: report is required"},
	} {
		p := mustPlan(t, "FROM xo://t\nCONNECT ssh --user=u\n"+tt.step+"\n")
		_, err := CheckPlugins(context.Background(), p, t.TempDir(), nil)
		if (tt.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.err)) {
			t.Errorf("%s: err = %v, want %q", tt.step, err, tt.err)
		}
	}
}
