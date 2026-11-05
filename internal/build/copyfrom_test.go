package build

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/littlejo/opendango/internal/plan"
)

// TestCopyFrom runs COPY --from with the local shell: the build stage
// writes files, which are fetched with tar at its end, then uploaded into
// the app stage (a directory's contents, like COPY; symbolic links stay
// links).
func TestCopyFrom(t *testing.T) {
	for _, builder := range []string{"xo://t", "plugin://opendangotest-local"} {
		t.Run(builder, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			from := func(root string) string {
				if strings.HasPrefix(builder, "plugin://") {
					return "FROM --root=" + root + " " + builder
				}
				return "FROM " + builder
			}
			dangofile := fmt.Sprintf(`%[3]s AS build
CONNECT ssh --user=tester
RUN mkdir -p %[1]s/sub && echo bin > %[1]s/app && chmod 0750 %[1]s/app && echo conf > %[1]s/sub/app.conf && ln -s app.conf %[1]s/sub/link && ln -s gone %[1]s/sub/dangling

%[4]s AS app
CONNECT ssh --user=tester
COPY --from=build %[1]s/app %[2]s/bin/
COPY --from=build %[1]s/sub %[2]s/etc/
`, src, dst, from(t.TempDir()), from(t.TempDir()))
			var log syncBuffer
			opts := Options{ContextDir: t.TempDir(), Out: &log, NoCache: true, FindPlugin: testPlugin(t), NewBuilder: only(&fakeBuilder{}), Dial: dialTo(newLocalComm(t))}
			if err := Run(context.Background(), mustPlan(t, dangofile), opts); err != nil {
				t.Fatalf("%v\n%s", err, log.String())
			}
			for p, want := range map[string]string{"bin/app": "bin\n", "etc/app.conf": "conf\n"} {
				if got := readFile(t, filepath.Join(dst, p)); got != want {
					t.Errorf("%s = %q, want %q", p, got, want)
				}
			}
			if info, err := os.Stat(filepath.Join(dst, "bin/app")); err != nil || info.Mode().Perm() != 0o750 {
				t.Errorf("bin/app: %v %v", info, err)
			}
			for name, want := range map[string]string{"etc/link": "app.conf", "etc/dangling": "gone"} {
				if target, err := os.Readlink(filepath.Join(dst, name)); err != nil || target != want {
					t.Errorf("%s -> %q, %v; want %q", name, target, err, want)
				}
			}
			if !strings.Contains(log.String(), "==> build: Fetching "+src+"/app "+src+"/sub for COPY --from=build") {
				t.Errorf("log:\n%s", log.String())
			}
		})
	}
}

func TestCopyFromMissing(t *testing.T) {
	dangofile := "FROM xo://t AS build\nCONNECT ssh --user=tester\nRUN true\n\nFROM xo://t AS app\nCONNECT ssh --user=tester\nCOPY --from=build /nonexistent/opendango /tmp/x\n"
	var log syncBuffer
	err := Run(context.Background(), mustPlan(t, dangofile), Options{Out: &log, NoCache: true, NewBuilder: only(&fakeBuilder{}), Dial: dialTo(newLocalComm(t))})
	if err == nil || !strings.Contains(err.Error(), "fetching /nonexistent/opendango for COPY --from") || strings.Contains(log.String(), "==> app:") {
		t.Errorf("err = %v\n%s", err, log.String())
	}
}

// TestUntar checks that an entry cannot land outside the copy.
func TestUntar(t *testing.T) {
	for _, name := range []string{"../evil", "a/../../evil"} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
		tw.Write([]byte("x"))
		tw.Close()
		root := t.TempDir()
		err := untar(&buf, filepath.Join(root, "copy"))
		if _, serr := os.Stat(filepath.Join(root, "evil")); serr == nil {
			t.Errorf("%s: written outside the copy (err %v)", name, err)
		}
	}
}

// TestCopyFromFilesCache checks the files cache: a stage that only feeds
// COPY --from is built once, then its files come from the cache.
func TestCopyFromFilesCache(t *testing.T) {
	src, dst, cache := t.TempDir(), t.TempDir(), t.TempDir()
	dangofile := fmt.Sprintf("FROM xo://t AS build\nCONNECT ssh --user=tester\nRUN date +%%s%%N > %[1]s/app\n\nFROM xo://t AS app\nCONNECT ssh --user=tester\nCOPY --from=build %[1]s/app %[2]s/app\n", src, dst)
	build := func() string {
		t.Helper()
		var log syncBuffer
		opts := Options{Out: &log, FilesCache: cache, NewBuilder: only(&fakeBuilder{}), Dial: dialTo(newLocalComm(t))}
		if err := Run(context.Background(), mustPlan(t, dangofile), opts); err != nil {
			t.Fatalf("%v\n%s", err, log.String())
		}
		return log.String()
	}
	log := build()
	first := readFile(t, filepath.Join(dst, "app"))
	if strings.Contains(log, "Using the cached files") {
		t.Errorf("first build:\n%s", log)
	}
	log = build()
	if !strings.Contains(log, "==> build: Using the cached files of COPY --from (key ") || strings.Contains(log, "==> build: RUN") {
		t.Errorf("second build:\n%s", log)
	}
	if got := readFile(t, filepath.Join(dst, "app")); got != first {
		t.Errorf("second build copied %q, want the cached %q", got, first)
	}
}

// TestCopyFromCachedTemplate checks a COPY --from added after the source
// stage was cached as a template: a VM starts from the cached template only
// to fetch the files; the next build finds them in the files cache.
func TestCopyFromCachedTemplate(t *testing.T) {
	src, dst, cache := t.TempDir(), t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(src, "app"), []byte("from the template\n"), 0o644)
	dangofile := fmt.Sprintf("FROM xo://t AS build\nCONNECT ssh --user=tester\nRUN false\nOUTPUT --as=template golden\n\nFROM xo://t AS app\nCONNECT ssh --user=tester\nCOPY --from=build %[1]s/app %[2]s/app\n", src, dst)
	p := mustPlan(t, dangofile)
	key, err := cacheKey(p.File, p.Stages[0], "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	build := func() (string, *fakeBuilder) {
		t.Helper()
		b := &fakeBuilder{cache: map[string]string{key: "tpl-build"}}
		var log syncBuffer
		opts := Options{Out: &log, FilesCache: cache, NewBuilder: only(b), Dial: dialTo(newLocalComm(t))}
		if err := Run(context.Background(), p, opts); err != nil {
			t.Fatalf("%v\n%s", err, log.String())
		}
		return log.String(), b
	}
	// RUN false: build is never built again, its template is in the cache.
	log, b := build()
	if !strings.Contains(log, "==> build: The files of COPY --from are not in the cache: fetching them from the cached template tpl-build") || !contains(b.calls, "destroy") {
		t.Errorf("first build: calls %q\n%s", b.calls, log)
	}
	if got := readFile(t, filepath.Join(dst, "app")); got != "from the template\n" {
		t.Errorf("app = %q", got)
	}
	log, _ = build()
	if strings.Contains(log, "fetching them from the cached template") || !strings.Contains(log, "==> build: Using the cached template tpl-build") {
		t.Errorf("second build:\n%s", log)
	}
}

// TestPackerCopyFromCachedTemplate is TestCopyFromCachedTemplate with a
// builder of the table (the test plugin as xenorchestra): the plugin builds
// a machine from the cached template to fetch the files, then its artifact
// is deleted.
func TestPackerCopyFromCachedTemplate(t *testing.T) {
	t.Setenv("OPENDANGO_TEST_STORE", t.TempDir())
	fetchRoot := t.TempDir()
	t.Setenv("OPENDANGO_TEST_ROOT", fetchRoot)
	src, dst, cache, base := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(src, "app"), []byte("built once\n"), 0o644)
	stage := fmt.Sprintf("FROM --root=%s plugin://xenorchestra AS build\nCONNECT ssh --user=tester\nRUN true\nOUTPUT --as=template golden\n", base)
	consumer := fmt.Sprintf("\nFROM --root=%s plugin://opendangotest-local AS app\nCONNECT ssh --user=tester\nCOPY --from=build %s/app %s/app\n", t.TempDir(), src, dst)
	run := func(dangofile string) string {
		t.Helper()
		var log syncBuffer
		if err := Run(context.Background(), mustPlan(t, dangofile), Options{Out: &log, FilesCache: cache, FindPlugin: testPlugin(t)}); err != nil {
			t.Fatalf("%v\n%s", err, log.String())
		}
		return log.String()
	}
	// The template is cached, without files: nobody copied from it.
	run(stage)
	log := run(stage + consumer)
	if !strings.Contains(log, "==> build: The files of COPY --from are not in the cache: fetching them from the cached template "+base) {
		t.Errorf("log:\n%s", log)
	}
	if got := readFile(t, filepath.Join(dst, "app")); got != "built once\n" {
		t.Errorf("app = %q", got)
	}
	// The machine of the fetch started from the cached template, and its
	// artifact is gone.
	if !strings.Contains(log, "==> build: source: template="+base+"\n") {
		t.Errorf("not from the cached template:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(fetchRoot, "artifact-destroyed")); err != nil {
		t.Errorf("the artifact of the fetch was not deleted: %v", err)
	}
	if log := run(stage + consumer); strings.Contains(log, "fetching them") {
		t.Errorf("third build fetched again:\n%s", log)
	}
}

// TestCopyFromCacheKey checks that a COPY --from is keyed by its source
// stage's key: the source changes, the consumer's key changes; a source
// without a key leaves the consumer without one.
func TestCopyFromCacheKey(t *testing.T) {
	key := func(run string) string {
		t.Helper()
		p := mustPlan(t, "FROM xo://t AS build\nCONNECT ssh --user=u\nRUN "+run+"\n\nFROM xo://t AS app\nCONNECT ssh --user=u\nCOPY --from=build /opt/app /usr/local/bin/\nOUTPUT --as=template app\n")
		keys := map[*plan.Stage]string{}
		for _, s := range p.Stages {
			k, err := stageKey(p.File, s, keys, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			keys[s] = k
		}
		return keys[p.Stages[1]]
	}
	a, b := key("make"), key("make install")
	if a == "" || a == b {
		t.Errorf("keys %q and %q", a, b)
	}
	// BREAKPOINT: the source has no key, nor has the consumer.
	if k := key("true\nBREAKPOINT"); k != "" {
		t.Errorf("key %q, want none", k)
	}
}
