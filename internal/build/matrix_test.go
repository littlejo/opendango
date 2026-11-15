package build

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/littlejo/opendango/internal/parser"
	"github.com/littlejo/opendango/internal/plan"
)

func matrixPlans(t *testing.T, src string, values ...string) []Variant {
	t.Helper()
	res, err := parser.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	var variants []Variant
	for _, v := range values {
		p, err := plan.Build("Dangofile", res, map[string]string{"FLAVOR": v})
		if err != nil {
			t.Fatal(err)
		}
		variants = append(variants, Variant{Name: "FLAVOR=" + v, Values: map[string]string{"FLAVOR": v}, Plan: p})
	}
	return variants
}

// TestRunMatrix builds two variants that share their first stage: it is
// built once, the other variant takes it from the cache; each builds its
// own second stage, and the manifest gets both, told apart by custom_data.
func TestRunMatrix(t *testing.T) {
	root, ctxDir, out := t.TempDir(), t.TempDir(), t.TempDir()
	for _, d := range []string{"base", "app-a", "app-b"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	src := fmt.Sprintf(`ARG FLAVOR
FROM --root=%[1]s/base plugin://qemu AS base
CONNECT ssh --user=u
RUN echo base > base.txt

FROM --root=%[1]s/app-${FLAVOR} --output_directory=%[2]s/${FLAVOR} base AS app
ARG FLAVOR
RUN echo ${FLAVOR} > flavor.txt
OUTPUT --format=checksum SUMS-${FLAVOR}
OUTPUT --format=manifest manifest.json
`, root, out)
	var log syncBuffer
	opts := Options{ContextDir: ctxDir, Out: &log, FindPlugin: testPlugin(t), ImagesCache: t.TempDir(), FilesCache: t.TempDir()}
	if err := RunMatrix(context.Background(), matrixPlans(t, src, "a", "b"), opts); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	l := log.String()
	if n := strings.Count(l, "==> base: Using the cached image"); n != 1 || !strings.Contains(l, "==> base: Waiting for the same stage in another variant") {
		t.Errorf("base built %d time(s) from the cache, want 1:\n%s", n, l)
	}
	for _, f := range []string{"a", "b"} {
		if !strings.Contains(l, "[FLAVOR="+f+"] ==> app:") {
			t.Errorf("no log of app in variant %s:\n%s", f, l)
		}
		if got := readFile(t, filepath.Join(root, "app-"+f, "flavor.txt")); got != f+"\n" {
			t.Errorf("flavor %s: %q", f, got)
		}
		if got := readFile(t, filepath.Join(ctxDir, "SUMS-"+f)); !strings.Contains(got, filepath.Join(out, f, "disk.qcow2")) {
			t.Errorf("SUMS-%s = %q", f, got)
		}
	}
	var mf manifestFile
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(ctxDir, "manifest.json"))), &mf); err != nil {
		t.Fatal(err)
	}
	if len(mf.Builds) != 2 || mf.Builds[0].PackerRunUUID != mf.Builds[1].PackerRunUUID {
		t.Fatalf("manifest %+v", mf)
	}
	flavors := map[any]bool{}
	for _, b := range mf.Builds {
		flavors[b.CustomData["FLAVOR"]] = true
	}
	if !flavors["a"] || !flavors["b"] {
		t.Errorf("custom_data %+v", mf.Builds)
	}
}

// TestRunMatrixFailure: a failed variant does not stop the other one.
func TestRunMatrixFailure(t *testing.T) {
	root, ctxDir := t.TempDir(), t.TempDir()
	src := fmt.Sprintf(`FROM --root=%s plugin://opendangotest-local
CONNECT ssh --user=u
ARG FLAVOR
RUN test "${FLAVOR}" = good && echo ok > ${FLAVOR}.txt
`, root)
	var log syncBuffer
	opts := Options{ContextDir: ctxDir, Out: &log, FindPlugin: testPlugin(t), NoCache: true}
	err := RunMatrix(context.Background(), matrixPlans(t, src, "bad", "good"), opts)
	if err == nil || !strings.HasPrefix(err.Error(), "[FLAVOR=bad] ") || strings.Contains(err.Error(), "FLAVOR=good") {
		t.Fatalf("error %v\n%s", err, log.String())
	}
	if got := readFile(t, filepath.Join(root, "good.txt")); got != "ok\n" {
		t.Errorf("good.txt = %q", got)
	}
}

func TestCheckMatrix(t *testing.T) {
	for _, tt := range []struct{ src, err string }{
		{"FROM xo://t\nOUTPUT --format=xva out.xva\n", "[FLAVOR=a] and [FLAVOR=b] both write " + filepath.Join("/ctx", "out.xva") + ": use a matrix variable in it"},
		{"FROM xo://t\nOUTPUT --as=template base\n", "both write the template base"},
		{"FROM xo://t\nCONNECT ssh --user=u\nFETCH /etc/os-release fetched/\n", "both write " + filepath.Join("/ctx", "fetched")},
		{"FROM xo://t\nOUTPUT --format=checksum /abs/SUMS\n", "both write /abs/SUMS"},
		// The manifests are shared; a variable makes the names differ.
		{"FROM xo://t\nOUTPUT --as=template\nOUTPUT --format=manifest m.json\n", ""},
		{"FROM xo://t\nARG FLAVOR\nOUTPUT --as=template base-${FLAVOR}\nOUTPUT --format=xva ${FLAVOR}.xva\n", ""},
	} {
		err := checkMatrix(matrixPlans(t, "ARG FLAVOR\n"+tt.src, "a", "b"), Options{ContextDir: "/ctx"})
		if (tt.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.err)) {
			t.Errorf("%q: error %v, want %q", tt.src, err, tt.err)
		}
	}
}
