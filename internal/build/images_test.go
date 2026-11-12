package build

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/littlejo/opendango/internal/parser"
	"github.com/littlejo/opendango/internal/plan"
)

// qemuStages is a parent stage and a child that starts from its disk, with
// the test plugin as plugin://qemu.
func qemuStages(root, out string) string {
	return fmt.Sprintf(`FROM --root=%[1]s/base plugin://qemu AS base
CONNECT ssh --user=u
RUN echo base > base.txt

FROM --root=%[1]s/app --output_directory=%[2]s base AS app
RUN echo app > app.txt
OUTPUT --format=checksum SUMS
`, root, out)
}

// TestFilesArtifact builds stages of a builder whose artifact is files: the
// parent's goes to the images cache, the child starts from its disk (the
// largest file) and its own stays where its settings put it; the next
// build takes the parent from the cache.
func TestFilesArtifact(t *testing.T) {
	root, ctxDir, images := t.TempDir(), t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(root, "base"), 0o755)
	os.MkdirAll(filepath.Join(root, "app"), 0o755)
	out := filepath.Join(t.TempDir(), "out")
	opts := func(log *syncBuffer) Options {
		return Options{ContextDir: ctxDir, Out: log, FindPlugin: testPlugin(t), ImagesCache: images, FilesCache: t.TempDir()}
	}
	var log syncBuffer
	if err := Run(context.Background(), mustPlan(t, qemuStages(root, out)), opts(&log)); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	entries, _ := ListImages(images)
	if len(entries) != 1 {
		t.Fatalf("images cache: %+v\n%s", entries, log.String())
	}
	cached := filepath.Join(entries[0].Dir, "disk.qcow2")
	if !strings.Contains(log.String(), "disk: iso_url="+cached+" iso_checksum=none disk_image=true") {
		t.Errorf("app does not start from the cached disk:\n%s", log.String())
	}
	if got := readFile(t, filepath.Join(out, "disk.qcow2")); got != "disk of "+root+"/app from [disk of "+root+"/base]" {
		t.Errorf("app disk = %q", got)
	}
	if got := readFile(t, filepath.Join(ctxDir, "SUMS")); !strings.Contains(got, filepath.Join(out, "disk.qcow2")) {
		t.Errorf("SUMS = %q", got)
	}

	// The next build: base from the cache, app built again (its own
	// output directory is new).
	os.RemoveAll(out)
	log = syncBuffer{}
	if err := Run(context.Background(), mustPlan(t, qemuStages(root, out)), opts(&log)); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "==> base: Using the cached image "+entries[0].Dir) || strings.Contains(log.String(), "base: Running the instructions") {
		t.Errorf("base not cached:\n%s", log.String())
	}
	if _, err := os.Stat(filepath.Join(out, "disk.qcow2")); err != nil {
		t.Errorf("app: %v", err)
	}
}

// TestFilesArtifactNoCache: without the cache, the parent's artifact is in
// a temporary directory, deleted at the end of the build.
func TestFilesArtifactNoCache(t *testing.T) {
	root, ctxDir := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(root, "base"), 0o755)
	os.MkdirAll(filepath.Join(root, "app"), 0o755)
	out := filepath.Join(t.TempDir(), "out")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	var log syncBuffer
	if err := Run(context.Background(), mustPlan(t, qemuStages(root, out)), Options{ContextDir: ctxDir, Out: &log, FindPlugin: testPlugin(t), NoCache: true}); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "disk: iso_url="+filepath.Join(tmp, "opendango-base-")) {
		t.Errorf("app does not start from the temporary disk:\n%s", log.String())
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "opendango-base-*")); len(left) != 0 {
		t.Errorf("temporary directories left: %v", left)
	}
	if _, err := os.Stat(filepath.Join(out, "disk.qcow2")); err != nil {
		t.Errorf("app: %v", err)
	}
}

func TestFilesArtifactPlan(t *testing.T) {
	for _, tt := range []struct{ src, err string }{
		{"FROM plugin://qemu\nOUTPUT --as=template x\n", "Dangofile:2: OUTPUT --as=template has no meaning in a plugin://qemu stage"},
		{"FROM plugin://qemu\nLABEL a=b\n", "Dangofile:2: LABEL is not supported in a plugin://qemu stage"},
		{"FROM --output_directory=x plugin://qemu AS a\nFROM a\n", "Dangofile:1: --output_directory cannot be set for stage a"},
		{"FROM plugin://qemu AS a\nFROM --iso_url=x a\n", "--iso_url cannot be set when starting from another stage"},
	} {
		res, err := parser.Parse(strings.NewReader(tt.src))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := plan.Build("Dangofile", res, nil); err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("%q: error %v, want %q", tt.src, err, tt.err)
		}
	}
	// No warning for a stage without OUTPUT: its files stay.
	p := mustPlan(t, "FROM plugin://qemu\n")
	if len(p.Warnings) != 0 {
		t.Errorf("warnings %q", p.Warnings)
	}
}
