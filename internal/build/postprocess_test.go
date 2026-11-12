package build

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/littlejo/opendango/builders/packer"
	"github.com/littlejo/opendango/internal/parser"
	"github.com/littlejo/opendango/internal/plan"
)

// TestPostProcessNative checks the post-processors of opendango on a native
// stage: checksum and archives of its XVA, and a manifest that keeps the
// builds of the previous runs.
func TestPostProcessNative(t *testing.T) {
	ctxDir := t.TempDir()
	src := `FROM xo://t AS golden
OUTPUT --format=xva dist/golden.xva
OUTPUT --as=template golden
OUTPUT --format=checksum dist/SHA256SUMS
OUTPUT --format=compress dist/golden.tar.gz
OUTPUT --format=compress dist/golden.zip
OUTPUT --format=manifest dist/manifest.json
`
	for range 2 {
		var log syncBuffer
		if err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: ctxDir, Out: &log, NoCache: true, NewBuilder: only(&fakeBuilder{id: "tpl-1"})}); err != nil {
			t.Fatalf("%v\n%s", err, log.String())
		}
	}
	xva := readFile(t, filepath.Join(ctxDir, "dist/golden.xva"))
	sum := sha256.Sum256([]byte(xva))
	if got, want := readFile(t, filepath.Join(ctxDir, "dist/SHA256SUMS")), hex.EncodeToString(sum[:])+"  golden.xva\n"; got != want {
		t.Errorf("SHA256SUMS = %q, want %q", got, want)
	}
	if got := tarGzFiles(t, filepath.Join(ctxDir, "dist/golden.tar.gz")); got["golden.xva"] != xva || len(got) != 1 {
		t.Errorf("tar.gz: %q", got)
	}
	if got := zipFiles(t, filepath.Join(ctxDir, "dist/golden.zip")); got["golden.xva"] != xva || len(got) != 1 {
		t.Errorf("zip: %q", got)
	}
	var m manifestFile
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(ctxDir, "dist/manifest.json"))), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Builds) != 2 || m.Builds[0].PackerRunUUID == m.Builds[1].PackerRunUUID || m.LastRunUUID != m.Builds[1].PackerRunUUID {
		t.Fatalf("manifest %+v", m)
	}
	b := m.Builds[1]
	if b.Name != "golden" || b.BuilderType != "xo" || b.ArtifactID != "tpl-1" || len(b.Files) != 1 || b.Files[0].Name != "dist/golden.xva" || b.Files[0].Size != int64(len(xva)) {
		t.Errorf("build %+v", b)
	}
}

// TestPostProcessCacheHit checks that the post-processors run on a stage
// found in the cache, on the cached template.
func TestPostProcessCacheHit(t *testing.T) {
	ctxDir := t.TempDir()
	p := mustPlan(t, "FROM xo://t AS golden\nOUTPUT --as=template golden\nOUTPUT --format=manifest m.json\n")
	key, err := cacheKey(p.File, p.Stages[0], "", ctxDir)
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBuilder{cache: map[string]string{key: "tpl-cached"}}
	var log syncBuffer
	if err := Run(context.Background(), p, Options{ContextDir: ctxDir, Out: &log, NewBuilder: only(b)}); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if !strings.Contains(readFile(t, filepath.Join(ctxDir, "m.json")), `"artifact_id": "tpl-cached"`) || contains(b.calls, "start") {
		t.Errorf("calls %q\n%s", b.calls, readFile(t, filepath.Join(ctxDir, "m.json")))
	}
}

// TestPostProcessPlugin checks the post-processors on a plugin:// stage of a
// plugin that is not in the table: its artifact's files, a plugin's
// post-processor that gets the artifact (id, builder id, files) and its
// dango.yaml section.
func TestPostProcessPlugin(t *testing.T) {
	root, ctxDir := t.TempDir(), t.TempDir()
	yaml := filepath.Join(ctxDir, "dango.yaml")
	os.WriteFile(yaml, []byte("opendangotest-inspect:\n  note: from dango.yaml\n"), 0o644)
	settings, err := plan.LoadSettings(yaml)
	if err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf("FROM --root=%s plugin://opendangotest-local AS golden\nOUTPUT --format=checksum --type=md5 SUMS\nOUTPUT --output=%s/inspect.txt plugin://opendangotest-inspect\n", root, ctxDir)
	res, err := parser.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.BuildSettings("Dangofile", res, nil, settings)
	if err != nil {
		t.Fatal(err)
	}
	var log syncBuffer
	find := testPlugin(t)
	findPP := func(ctx context.Context, source string) (*packer.Plugin, string, error) {
		pl, _, err := find(ctx, "opendangotest-local")
		return pl, strings.TrimPrefix(source, "opendangotest-"), err
	}
	if err := Run(context.Background(), p, Options{ContextDir: ctxDir, Out: &log, FindPlugin: find, FindPostProcessor: findPP}); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	image := filepath.Join(root, "image.qcow2")
	if got := readFile(t, filepath.Join(ctxDir, "SUMS")); !strings.HasSuffix(got, "  "+image+"\n") || len(got) != 32+2+len(image)+1 {
		t.Errorf("SUMS = %q", got)
	}
	if got, want := readFile(t, filepath.Join(ctxDir, "inspect.txt")), "id="+root+"\nbuilder=opendango.test\nfiles="+image+"\nnote=from dango.yaml\n"; got != want {
		t.Errorf("inspect.txt = %q, want %q", got, want)
	}
}

// TestPostProcessChain chains post-processors: each --chain one gets the
// artifact of the previous one, which goes unless it keeps its input.
func TestPostProcessChain(t *testing.T) {
	root, ctxDir := t.TempDir(), t.TempDir()
	src := fmt.Sprintf(`FROM --root=%[1]s plugin://opendangotest-local AS golden
OUTPUT --output=%[2]s/a.txt plugin://opendangotest-inspect
OUTPUT --chain --output=%[2]s/b.txt --keep=false plugin://opendangotest-inspect
OUTPUT --chain --format=checksum SUMS
OUTPUT --format=compress --chain all.tar
OUTPUT --chain --output=%[2]s/c.txt --keep=false --keep_input_artifact=true plugin://opendangotest-inspect
OUTPUT --output=%[2]s/d.txt plugin://opendangotest-inspect
`, root, ctxDir)
	p := mustPlan(t, src)
	var log syncBuffer
	find := testPlugin(t)
	findPP := func(ctx context.Context, source string) (*packer.Plugin, string, error) {
		pl, _, err := find(ctx, "opendangotest-local")
		return pl, strings.TrimPrefix(source, "opendangotest-"), err
	}
	if err := Run(context.Background(), p, Options{ContextDir: ctxDir, Out: &log, FindPlugin: find, FindPostProcessor: findPP}); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	at := func(name string) string { return filepath.Join(ctxDir, name) }
	// b.txt got a.txt, then deleted it (keep=false).
	if got := readFile(t, at("b.txt")); !strings.Contains(got, "id=inspected:"+at("a.txt")+"\nbuilder=opendango.test.inspect\nfiles="+at("a.txt")+"\n") {
		t.Errorf("b.txt = %q", got)
	}
	if _, err := os.Stat(at("a.txt")); !os.IsNotExist(err) {
		t.Errorf("a.txt not deleted: %v", err)
	}
	// The checksum is b.txt's; the archive holds SUMS.
	if got := readFile(t, at("SUMS")); !strings.HasSuffix(got, "  b.txt\n") {
		t.Errorf("SUMS = %q", got)
	}
	// keep_input_artifact=true wins over the post-processor's keep=false.
	if got := readFile(t, at("c.txt")); !strings.Contains(got, "files="+at("all.tar")+"\n") {
		t.Errorf("c.txt = %q", got)
	}
	if _, err := os.Stat(at("all.tar")); err != nil {
		t.Errorf("all.tar: %v", err)
	}
	// Without --chain, the stage's artifact.
	if got := readFile(t, at("d.txt")); !strings.Contains(got, "id="+root+"\n") {
		t.Errorf("d.txt = %q", got)
	}
}

// TestPostProcessDestroyed checks that a known builder's artifact deleted
// for lack of OUTPUT --as=template is post-processed before, and that a
// checksum of an artifact without files is an error.
func TestPostProcessDestroyed(t *testing.T) {
	root, ctxDir := t.TempDir(), t.TempDir()
	src := fmt.Sprintf("FROM --root=%s plugin://xenorchestra\nOUTPUT --format=manifest m.json\n", root)
	var log syncBuffer
	if err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: ctxDir, Out: &log, NoCache: true, FindPlugin: testPlugin(t)}); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if !strings.Contains(readFile(t, filepath.Join(ctxDir, "m.json")), `"artifact_id": "`+root+`"`) {
		t.Errorf("manifest:\n%s", readFile(t, filepath.Join(ctxDir, "m.json")))
	}
	if _, err := os.Stat(filepath.Join(root, "artifact-destroyed")); err != nil {
		t.Errorf("not deleted: %v", err)
	}

	err := Run(context.Background(), mustPlan(t, "FROM xo://t\nOUTPUT --as=template x\nOUTPUT --format=checksum SUMS\n"), Options{ContextDir: ctxDir, NoCache: true, NewBuilder: only(&fakeBuilder{id: "tpl"})})
	if err == nil || !strings.Contains(err.Error(), "Dangofile:3: OUTPUT: the artifact of stage stage-0 has no file (tpl)") {
		t.Errorf("err = %v", err)
	}
}

func tarGzFiles(t *testing.T, p string) map[string]string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		files[h.Name] = string(b)
	}
}

func zipFiles(t *testing.T, p string) map[string]string {
	t.Helper()
	zr, err := zip.OpenReader(p)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	files := map[string]string{}
	for _, f := range zr.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r)
		r.Close()
		files[f.Name] = string(b)
	}
	return files
}

// TestCheckPostProcessors checks that validate has the plugins'
// post-processors check their settings (Configure).
func TestCheckPostProcessors(t *testing.T) {
	testPlugin(t)
	t.Setenv("PACKER_PLUGIN_PATH", filepath.Dir(pluginPath))
	for _, tt := range []struct{ out, err string }{
		{"OUTPUT --output=x --note=n plugin://opendangotest-inspect", ""},
		{"OUTPUT --output=x --bogus=1 plugin://opendangotest-inspect", "Dangofile:2: plugin://opendangotest-inspect: "},
		{"OUTPUT --output=x plugin://opendangotest-nothing", ""},
	} {
		p := mustPlan(t, "FROM xo://t\n"+tt.out+"\n")
		warnings, err := CheckPlugins(context.Background(), p, t.TempDir(), Finders{})
		if (tt.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.err)) {
			t.Errorf("%s: err = %v, want %q", tt.out, err, tt.err)
		}
		if strings.Contains(tt.out, "nothing") && !strings.Contains(strings.Join(warnings, "\n"), `has no post-processor "nothing"`) {
			t.Errorf("warnings %q", warnings)
		}
	}
}

// TestManifestBuildID checks that the manifest's run UUID is the build's
// DANGO_BUILD_ID.
func TestManifestBuildID(t *testing.T) {
	saved := plan.Builtins
	t.Cleanup(func() { plan.Builtins = saved })
	plan.Builtins = func(string) map[string]string { return map[string]string{plan.BuildIDVar: "build-42"} }
	ctxDir := t.TempDir()
	if err := Run(context.Background(), mustPlan(t, "FROM xo://t\nOUTPUT --format=manifest m.json\n"), Options{ContextDir: ctxDir, NoCache: true, NewBuilder: only(&fakeBuilder{})}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(ctxDir, "m.json")); !strings.Contains(got, `"last_run_uuid": "build-42"`) {
		t.Errorf("manifest:\n%s", got)
	}
}
