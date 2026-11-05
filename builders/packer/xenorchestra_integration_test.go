//go:build integration

package packer_test

// End-to-end test of plugin://xenorchestra (github.com/littlejo/packer-plugin-xenorchestra),
// the builder of the table of known builders: OUTPUT and LABEL become its
// settings, and its template is deleted without OUTPUT --as=template.
//
// Required environment: the plugin, where the discovery finds it
// (PACKER_PLUGIN_PATH, or `opendango plugin pull
// github.com/littlejo/xenorchestra`), and the variables of the xo:// tests
// (XOA_*, OPENDANGO_TEST_TEMPLATE, OPENDANGO_TEST_USER, OPENDANGO_TEST_POOL,
// OPENDANGO_TEST_NETWORK).

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/littlejo/opendango/internal/build"
)

func TestXenorchestraEndToEnd(t *testing.T) {
	env(t, "XOA_ENDPOINT")
	env(t, "XOA_TOKEN")
	from := fmt.Sprintf("--template=%q --pool=%q --network=%q --cpus=2 --memory=2G",
		env(t, "OPENDANGO_TEST_TEMPLATE"), env(t, "OPENDANGO_TEST_POOL"), env(t, "OPENDANGO_TEST_NETWORK"))
	user := env(t, "OPENDANGO_TEST_USER")
	ctx := context.Background()
	if _, _, err := build.FindPlugin(ctx, "xenorchestra"); err != nil {
		t.Skipf("packer-plugin-xenorchestra: %v", err)
	}
	ctxDir := t.TempDir()
	stamp := time.Now().Unix()
	golden := fmt.Sprintf("opendango-it-xoplugin-%d", stamp)
	scratch := fmt.Sprintf("opendango-it-xoplugin-scratch-%d", stamp)

	// OUTPUT --as=template with a name, LABEL, and an XVA.
	src := fmt.Sprintf(`FROM %s --vm_name=%s-vm plugin://xenorchestra AS golden
CONNECT ssh --user=%s --timeout=10m
USER root
RUN test "$(id -u)" = 0
LABEL team=web stage=golden
OUTPUT --format=xva --compress=zstd out/golden.xva
OUTPUT --as=template %s

FROM %s --vm_name=%s plugin://xenorchestra AS scratch
CONNECT ssh --user=%s --timeout=10m
RUN true
`, from, golden, user, golden, from, scratch, user)
	var log bytes.Buffer
	run(ctx, t, src, ctxDir, nil, io.MultiWriter(os.Stdout, &log))
	defer deleteXOTemplate(t, golden)

	templates := xoTemplates(t)
	var found bool
	for _, tmpl := range templates {
		switch tmpl.NameLabel {
		case golden:
			found = true
			for _, tag := range []string{"team=web", "stage=golden"} {
				if !slices.Contains(tmpl.Tags, tag) {
					t.Errorf("template tags %q lack %q", tmpl.Tags, tag)
				}
			}
		case scratch:
			// No OUTPUT: the plugin's template is deleted.
			t.Errorf("template %s (%s) of the stage without OUTPUT is left", tmpl.NameLabel, tmpl.ID)
			deleteXOTemplate(t, scratch)
		}
	}
	if !found {
		t.Errorf("no template %s", golden)
	}
	info, err := os.Stat(filepath.Join(ctxDir, "out", "golden.xva"))
	if err != nil || info.Size() < 1<<20 {
		t.Errorf("XVA: %v %v", info, err)
	}
	if !strings.Contains(log.String(), "==> scratch: Deleting the artifact: no OUTPUT --as=template") {
		t.Error("the artifact of the stage without OUTPUT was not deleted")
	}
}

// TestXenorchestraMultiStageEndToEnd builds base (an intermediate
// template) then app from it, with plugin://xenorchestra: the file written
// in base is in app, ENV and USER carry over, the intermediate template is
// deleted at the end and app keeps its labels.
func TestXenorchestraMultiStageEndToEnd(t *testing.T) {
	env(t, "XOA_ENDPOINT")
	env(t, "XOA_TOKEN")
	from := fmt.Sprintf("--template=%q --pool=%q --network=%q --cpus=2 --memory=2G",
		env(t, "OPENDANGO_TEST_TEMPLATE"), env(t, "OPENDANGO_TEST_POOL"), env(t, "OPENDANGO_TEST_NETWORK"))
	user := env(t, "OPENDANGO_TEST_USER")
	ctx := context.Background()
	if _, _, err := build.FindPlugin(ctx, "xenorchestra"); err != nil {
		t.Skipf("packer-plugin-xenorchestra: %v", err)
	}
	app := fmt.Sprintf("opendango-it-xoplugin-app-%d", time.Now().Unix())
	src := fmt.Sprintf(`FROM %s plugin://xenorchestra AS base
CONNECT ssh --user=%s --timeout=10m
USER root
ENV STAGE_ENV=from-base
WORKDIR /opt/opendango
RUN echo base > from-base.txt

FROM --cpus=1 base AS app
RUN test "$(cat from-base.txt)" = base && test "$STAGE_ENV" = from-base && test "$(id -u)" = 0 && test "$(nproc)" = 1
LABEL team=web
OUTPUT --as=template %s
`, from, user, app)
	var log bytes.Buffer
	run(ctx, t, src, t.TempDir(), nil, io.MultiWriter(os.Stdout, &log))
	defer deleteXOTemplate(t, app)

	var found bool
	for _, tmpl := range xoTemplates(t) {
		switch {
		case tmpl.NameLabel == app:
			found = true
			if !slices.Contains(tmpl.Tags, "team=web") {
				t.Errorf("template tags %q lack team=web", tmpl.Tags)
			}
		case strings.HasPrefix(tmpl.NameLabel, "opendango-tmp-base-"):
			t.Errorf("intermediate template %s (%s) left", tmpl.NameLabel, tmpl.ID)
		}
	}
	if !found {
		t.Errorf("no template %s", app)
	}
	if !strings.Contains(log.String(), "==> base: Deleting intermediate template ") {
		t.Error("the intermediate template was not deleted")
	}
}

type xoTemplate struct {
	ID        string   `json:"id"`
	NameLabel string   `json:"name_label"`
	Tags      []string `json:"tags"`
}

// xoTemplates lists the templates through the XO REST API.
func xoTemplates(t *testing.T) []xoTemplate {
	t.Helper()
	client := &http.Client{}
	if os.Getenv("XOA_INSECURE") == "true" {
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(os.Getenv("XOA_ENDPOINT"), "/")+"/rest/v0/vm-templates?fields="+url.QueryEscape("id,name_label,tags"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "authenticationToken", Value: os.Getenv("XOA_TOKEN")})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var templates []xoTemplate
	if err := json.NewDecoder(resp.Body).Decode(&templates); err != nil {
		t.Fatal(err)
	}
	return templates
}
