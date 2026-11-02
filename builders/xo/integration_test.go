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

func TestBuildEndToEnd(t *testing.T) {
	env(t, "XOA_ENDPOINT")
	env(t, "XOA_TOKEN")
	template := env(t, "OPENDANGO_TEST_TEMPLATE")
	user := env(t, "OPENDANGO_TEST_USER")

	ctxDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ctxDir, "hello.txt"), []byte("hello from opendango\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 2G: the minimum static memory of the XO Hub cloud-init templates.
	flags := "--cpus=1 --memory=2G"
	if pool := os.Getenv("OPENDANGO_TEST_POOL"); pool != "" {
		flags += fmt.Sprintf(" --pool=%q", pool)
	}
	if n := os.Getenv("OPENDANGO_TEST_NETWORK"); n != "" {
		flags += fmt.Sprintf(" --network=%q", n)
	}
	if sr := os.Getenv("OPENDANGO_TEST_SR"); sr != "" {
		flags += fmt.Sprintf(" --sr=%q", sr)
	}
	name := fmt.Sprintf("opendango-it-%d", time.Now().Unix())
	src := fmt.Sprintf(`FROM %s --template=%q xo:// AS it
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
`, flags, template, user, name)

	res, err := parser.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build("Dangofile", res, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Minute)
	defer cancel()
	if err := build.Run(ctx, p, build.Options{ContextDir: ctxDir, Out: os.Stdout}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(ctxDir, "out", "it.xva"))
	if err != nil || info.Size() == 0 {
		t.Errorf("XVA not exported: %v", err)
	}

	client, err := xo.NewClientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	id, err := xo.FindTemplate(ctx, client, name)
	if err != nil {
		t.Fatalf("template %s not found: %v", name, err)
	}
	if err := xo.DeleteTemplate(ctx, client, id); err != nil {
		t.Errorf("deleting template %s: %v", id, err)
	}
}
