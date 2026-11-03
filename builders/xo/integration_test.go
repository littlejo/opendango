//go:build integration

package xo_test

// End-to-end test against a real Xen Orchestra. It creates a VM, runs the
// instructions over SSH, exports an XVA, converts the VM to a template and
// deletes that template.
//
// Required environment:
//   XOA_ENDPOINT, XOA_TOKEN (and XOA_INSECURE=true for a self-signed cert)
//   OPENDANGO_TEST_TEMPLATE  a cloud-init template with the guest tools
//   OPENDANGO_TEST_USER      the template's default cloud-init user (with sudo)
// Optional: OPENDANGO_TEST_POOL (required when the template exists in
// several pools), OPENDANGO_TEST_NETWORK, OPENDANGO_TEST_SR.
//
//   go test -tags integration -timeout 60m ./builders/xo/

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/littlejo/opendango/builders/xo"
	"github.com/littlejo/opendango/internal/build"
	"github.com/littlejo/opendango/internal/parser"
	"github.com/littlejo/opendango/internal/plan"
)

func env(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is not set", name)
	}
	return v
}

// setup returns the FROM flags and the CONNECT user from the environment,
// and a build context holding hello.txt. It skips when not configured.
func setup(t *testing.T) (from, user, ctxDir string) {
	t.Helper()
	env(t, "XOA_ENDPOINT")
	env(t, "XOA_TOKEN")
	template := env(t, "OPENDANGO_TEST_TEMPLATE")
	user = env(t, "OPENDANGO_TEST_USER")

	ctxDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(ctxDir, "hello.txt"), []byte("hello from opendango\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 2G: the minimum static memory of the XO Hub cloud-init templates.
	from = "--cpus=1 --memory=2G"
	if pool := os.Getenv("OPENDANGO_TEST_POOL"); pool != "" {
		from += fmt.Sprintf(" --pool=%q", pool)
	}
	if n := os.Getenv("OPENDANGO_TEST_NETWORK"); n != "" {
		from += fmt.Sprintf(" --network=%q", n)
	}
	if sr := os.Getenv("OPENDANGO_TEST_SR"); sr != "" {
		from += fmt.Sprintf(" --sr=%q", sr)
	}
	from += fmt.Sprintf(" --template=%q xo://", template)
	return from, user, ctxDir
}

// run builds src with opts and returns an XO client and a context for the
// checks. The log goes to stdout when opts.Out is nil.
func run(t *testing.T, src string, opts build.Options) (context.Context, *xo.Client) {
	t.Helper()
	res, err := parser.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build("Dangofile", res, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Minute)
	t.Cleanup(cancel)
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if err := build.Run(ctx, p, opts); err != nil {
		t.Fatal(err)
	}
	client, err := xo.NewClientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return ctx, client
}

// deleteTemplate deletes the template named name, failing if it is missing.
func deleteTemplate(ctx context.Context, t *testing.T, client *xo.Client, name string) {
	t.Helper()
	id, err := xo.FindTemplate(ctx, client, name)
	if err != nil {
		t.Fatalf("template %s not found: %v", name, err)
	}
	if err := xo.DeleteTemplate(ctx, client, id); err != nil {
		t.Errorf("deleting template %s: %v", id, err)
	}
}

func TestBuildEndToEnd(t *testing.T) {
	from, user, ctxDir := setup(t)
	name := fmt.Sprintf("opendango-it-%d", time.Now().Unix())
	src := fmt.Sprintf(`FROM %s AS it
CONNECT ssh --user=%s --timeout=15m
ENV --persist OPENDANGO_IT=1
USER root
WORKDIR /opt/opendango
COPY --chmod=0640 hello.txt ./
RUN test "$(cat /opt/opendango/hello.txt)" = "hello from opendango"
RUN grep -q '^OPENDANGO_IT="1"$' /etc/environment
RUN test -f /root/retried || { touch /root/retried; exit 1; }
REBOOT
BREAKPOINT checked by the test
RUN test -f /opt/opendango/hello.txt
OUTPUT --format=xva --compress=zstd out/it.xva
OUTPUT --as=template %s
`, from, user, name)

	// The first run of the `retried` RUN fails: --on-error=ask answers r.
	// At the BREAKPOINT, log in with the printed ssh command, as a user would.
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Fatal("the BREAKPOINT check needs the ssh client")
	}
	var log bytes.Buffer
	sshCmd := regexp.MustCompile(`(?m)^    (ssh -i .*)$`)
	breakpoint, retried := false, false
	stdin := readerFunc(func(p []byte) (int, error) {
		if strings.HasSuffix(log.String(), "exit? ") {
			retried = true
			return copy(p, "r\n"), nil
		}
		// This runs in the build's goroutine: t.Error, not t.Fatal.
		breakpoint = true
		// The last one: the key file of the earlier failure is removed.
		all := sshCmd.FindAllStringSubmatch(log.String(), -1)
		if all == nil {
			t.Error("no ssh command in the log")
			return copy(p, "\n"), nil
		}
		m := all[len(all)-1]
		out, err := exec.Command("/bin/sh", "-c", m[1]+" -o BatchMode=yes test -f /opt/opendango/hello.txt").CombinedOutput()
		if err != nil {
			t.Errorf("%s: %v\n%s", m[1], err, out)
		}
		return copy(p, "\n"), nil
	})

	ctx, client := run(t, src, build.Options{ContextDir: ctxDir, Out: io.MultiWriter(os.Stdout, &log), Stdin: stdin, Ask: true})
	if !breakpoint || !retried {
		t.Errorf("BREAKPOINT reached: %v, failed RUN retried: %v", breakpoint, retried)
	}
	info, err := os.Stat(filepath.Join(ctxDir, "out", "it.xva"))
	if err != nil || info.Size() == 0 {
		t.Errorf("XVA not exported: %v", err)
	}
	deleteTemplate(ctx, t, client, name)
}

// TestMultiStageEndToEnd builds two child stages in parallel (-j 2) from
// the intermediate template of their parent, and checks what a child
// inherits.
func TestMultiStageEndToEnd(t *testing.T) {
	from, user, ctxDir := setup(t)
	name := fmt.Sprintf("opendango-it-ms-%d", time.Now().Unix())
	sibling := name + "-sibling"
	src := fmt.Sprintf(`FROM %s AS base
CONNECT ssh --user=%s --timeout=15m
USER root
ENV STAGE_ENV=from-base
WORKDIR /opt/opendango
COPY hello.txt ./

FROM --cpus=2 base AS app
RUN test -f /opt/opendango/hello.txt
RUN test "$STAGE_ENV" = from-base && test "$(pwd)" = /opt/opendango && test "$(id -u)" = 0
RUN test "$(nproc)" = 2
OUTPUT --as=template %s

FROM base AS sibling
RUN test -f /opt/opendango/hello.txt
OUTPUT --as=template %s
`, from, user, name, sibling)

	var log bytes.Buffer
	ctx, client := run(t, src, build.Options{ContextDir: ctxDir, Out: io.MultiWriter(os.Stdout, &log), Jobs: 2})
	deleteTemplate(ctx, t, client, name)
	deleteTemplate(ctx, t, client, sibling)
	// With -j 2, the sibling starts before app is finished.
	if strings.Index(log.String(), "==> sibling: Starting VM") > strings.Index(log.String(), "==> app: Stage finished") {
		t.Error("app and sibling were not built in parallel")
	}
	tmp, err := xo.TemplatesWithPrefix(ctx, client, "opendango-tmp-base-")
	if err != nil {
		t.Fatal(err)
	}
	if len(tmp) != 0 {
		t.Errorf("intermediate templates left behind: %v", tmp)
	}
}

// readerFunc lets a test act when the build waits for input.
type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
