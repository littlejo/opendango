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
