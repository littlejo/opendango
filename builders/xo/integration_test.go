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
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// run builds src and returns an XO client and a context for the checks.
func run(t *testing.T, src, ctxDir string) (context.Context, *xo.Client) {
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
	if err := build.Run(ctx, p, build.Options{ContextDir: ctxDir, Out: os.Stdout}); err != nil {
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
REBOOT
RUN test -f /opt/opendango/hello.txt
OUTPUT --format=xva --compress=zstd out/it.xva
OUTPUT --as=template %s
`, from, user, name)

	ctx, client := run(t, src, ctxDir)
	info, err := os.Stat(filepath.Join(ctxDir, "out", "it.xva"))
	if err != nil || info.Size() == 0 {
		t.Errorf("XVA not exported: %v", err)
	}
	deleteTemplate(ctx, t, client, name)
}

// TestMultiStageEndToEnd builds a child stage from the intermediate
// template of its parent, and checks what the child inherits.
func TestMultiStageEndToEnd(t *testing.T) {
	from, user, ctxDir := setup(t)
	name := fmt.Sprintf("opendango-it-ms-%d", time.Now().Unix())
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
`, from, user, name)

	ctx, client := run(t, src, ctxDir)
	deleteTemplate(ctx, t, client, name)
	tmp, err := xo.TemplatesWithPrefix(ctx, client, "opendango-tmp-base-")
	if err != nil {
		t.Fatal(err)
	}
	if len(tmp) != 0 {
		t.Errorf("intermediate templates left behind: %v", tmp)
	}
}
