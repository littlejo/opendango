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
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/littlejo/opendango/internal/build"
	"github.com/littlejo/opendango/internal/parser"
	"github.com/littlejo/opendango/internal/plan"
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
	os.Mkdir(filepath.Join(ctxDir, "http"), 0o755)
	os.WriteFile(filepath.Join(ctxDir, "http", "hello.txt"), []byte("served\n"), 0o644)
	stamp := time.Now().Unix()
	golden := fmt.Sprintf("opendango-it-xoplugin-%d", stamp)
	scratch := fmt.Sprintf("opendango-it-xoplugin-scratch-%d", stamp)

	// OUTPUT --as=template with a name, LABEL, an XVA, SERVE (the plugin's
	// server, DANGO_HTTP_ADDR from its hook data) and REBOOT (through the
	// plugin's communicator).
	src := fmt.Sprintf(`FROM %s --vm_name=%s-vm plugin://xenorchestra AS golden
SERVE http
CONNECT ssh --user=%s --timeout=10m
USER root
RUN test "$(id -u)" = 0
RUN test "$(curl -fsS "http://$DANGO_HTTP_ADDR/hello.txt")" = served
RUN cat /proc/sys/kernel/random/boot_id > /root/boot-before
REBOOT
RUN test "$(cat /proc/sys/kernel/random/boot_id)" != "$(cat /root/boot-before)"
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
	if !strings.Contains(log.String(), "==> golden: REBOOT\n") {
		t.Error("log lacks REBOOT")
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

// TestXenorchestraCacheEndToEnd builds base then app three times with the
// cache: everything is built, then everything comes from the cache (the
// plugin's data source finds the tagged templates), then app changes and
// starts from the cached base. app never keeps the cache tags XO copies
// from base. Only the templates the builds report are deleted.
func TestXenorchestraCacheEndToEnd(t *testing.T) {
	env(t, "XOA_ENDPOINT")
	env(t, "XOA_TOKEN")
	from := fmt.Sprintf("--template=%q --pool=%q --network=%q --cpus=2 --memory=2G",
		env(t, "OPENDANGO_TEST_TEMPLATE"), env(t, "OPENDANGO_TEST_POOL"), env(t, "OPENDANGO_TEST_NETWORK"))
	user := env(t, "OPENDANGO_TEST_USER")
	ctx := context.Background()
	if _, _, err := build.FindPlugin(ctx, "xenorchestra"); err != nil {
		t.Skipf("packer-plugin-xenorchestra: %v", err)
	}
	stamp := time.Now().Unix()
	app := fmt.Sprintf("opendango-it-xoplugin-cache-%d", stamp)
	src := func(run string) string {
		return fmt.Sprintf(`FROM %s plugin://xenorchestra AS base
CONNECT ssh --user=%s --timeout=10m
USER root
RUN echo %d > /opt/opendango-cache-it

FROM base AS app
RUN %s
LABEL team=web
OUTPUT --format=xva --compress=zstd app.xva
OUTPUT --as=template %s
`, from, user, stamp, run, app)
	}
	// The templates the builds made, by id, from their logs.
	made := map[string]bool{}
	artifact := regexp.MustCompile(`==> \w+: Artifact: Xen Orchestra template \S+ \(([0-9a-f-]+)\)`)
	defer func() {
		for id := range made {
			deleteXOTemplateID(t, id)
		}
	}()
	ctxDir := t.TempDir()
	xva := filepath.Join(ctxDir, "app.xva")
	buildCached := func(src string) string {
		t.Helper()
		os.Remove(xva)
		var log bytes.Buffer
		res, err := parser.Parse(strings.NewReader(src))
		if err != nil {
			t.Fatal(err)
		}
		p, err := plan.Build("Dangofile", res, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = build.Run(ctx, p, build.Options{ContextDir: ctxDir, Out: io.MultiWriter(os.Stdout, &log)})
		for _, m := range artifact.FindAllStringSubmatch(log.String(), -1) {
			made[m[1]] = true
		}
		if err != nil {
			t.Fatal(err)
		}
		return log.String()
	}

	run := `test "$(cat /opt/opendango-cache-it)" = ` + fmt.Sprint(stamp)
	t.Log("first build: fills the cache")
	log := buildCached(src(run))
	if strings.Contains(log, "Using the cached template") {
		t.Errorf("first build used the cache")
	}
	t.Log("second build: everything from the cache")
	log = buildCached(src(run))
	for _, want := range []string{"==> base: Using the cached template ", "==> app: Using the cached template "} {
		if !strings.Contains(log, want) {
			t.Errorf("second build: log lacks %q", want)
		}
	}
	if strings.Contains(log, "Running the instructions") {
		t.Error("second build ran instructions")
	}
	// The XVA comes from the cached template.
	if info, err := os.Stat(xva); err != nil || info.Size() < 1<<20 {
		t.Errorf("XVA of the cached template: %v %v", info, err)
	}
	t.Log("third build: app changes, base comes from the cache")
	log = buildCached(src(run + " && true"))
	if !strings.Contains(log, "==> base: Using the cached template ") || !strings.Contains(log, "==> app: Running the instructions") {
		t.Errorf("third build:\n%s", log)
	}

	// Two app templates (first and third key), each with the label and its
	// own cache tag only.
	var apps int
	for _, tmpl := range xoTemplates(t) {
		if !made[tmpl.ID] || tmpl.NameLabel != app {
			continue
		}
		apps++
		var keys int
		for _, tag := range tmpl.Tags {
			if strings.HasPrefix(tag, "opendango-cache") {
				keys++
			}
		}
		if keys != 1 || !slices.Contains(tmpl.Tags, "team=web") {
			t.Errorf("template %s tags %q: want team=web and one cache tag", tmpl.ID, tmpl.Tags)
		}
	}
	if apps != 2 {
		t.Errorf("%d templates %s, want 2", apps, app)
	}
}

// deleteXOTemplateID deletes one template by id through the XO REST API.
func deleteXOTemplateID(t *testing.T, id string) {
	t.Helper()
	client := &http.Client{}
	if os.Getenv("XOA_INSECURE") == "true" {
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	req, err := http.NewRequest(http.MethodDelete, strings.TrimSuffix(os.Getenv("XOA_ENDPOINT"), "/")+"/rest/v0/vm-templates/"+url.PathEscape(id), nil)
	if err != nil {
		t.Error(err)
		return
	}
	req.AddCookie(&http.Cookie{Name: "authenticationToken", Value: os.Getenv("XOA_TOKEN")})
	resp, err := client.Do(req)
	if err != nil {
		t.Errorf("deleting template %s: %v", id, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Errorf("deleting template %s: %s", id, resp.Status)
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
