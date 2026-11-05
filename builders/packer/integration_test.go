//go:build integration

package packer_test

// End-to-end test of plugin:// with vatesfr/packer-plugin-xenserver, which
// talks to XAPI directly.
//
// The plugin's xva builder clones a template but cannot change the network
// of its existing VIF (DEVICE_ALREADY_EXISTS; only a message when
// network_names is set, an error otherwise): opendango's xo:// builder first
// makes a temporary template attached to the DHCP network. Then the
// plugin clones it; a cidata CD (cd_files) authorizes a key generated here;
// the plugin connects with it and calls opendango to run the instructions;
// format=none and keep_vm=never make it delete its VM.
//
// Two settings work around the xva builder of v0.11.4: --firmware (no
// default, unlike the iso builder) and --installtimeout (the IP wait,
// otherwise zero: install_timeout is only read by the iso builder).
//
// Required environment:
//   XAPI_ENDPOINT (https://<pool master>), XAPI_USERNAME, XAPI_PASSWORD
//   PACKER_PLUGIN_PATH  where packer-plugin-xenserver is
//   and the variables of the xo:// tests (builders/xo/integration_test.go),
//   OPENDANGO_TEST_POOL and OPENDANGO_TEST_NETWORK included: the pool of
//   the XAPI host, and a network with DHCP.
//
//   go test -tags integration -timeout 60m ./builders/packer/

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

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

func TestXenserverEndToEnd(t *testing.T) {
	endpoint := env(t, "XAPI_ENDPOINT")
	username := env(t, "XAPI_USERNAME")
	password := env(t, "XAPI_PASSWORD")
	env(t, "XOA_ENDPOINT")
	env(t, "XOA_TOKEN")
	template := env(t, "OPENDANGO_TEST_TEMPLATE")
	pool := env(t, "OPENDANGO_TEST_POOL")
	network := env(t, "OPENDANGO_TEST_NETWORK")
	user := env(t, "OPENDANGO_TEST_USER")
	host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://"), "/")
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Minute)
	defer cancel()

	// The template the plugin clones, made with xo://.
	base := fmt.Sprintf("opendango-it-packer-base-%d", time.Now().Unix())
	t.Cleanup(func() { deleteXOTemplate(t, base) })
	run(ctx, t, fmt.Sprintf("FROM --pool=%q --network=%q --template=%q --cpus=2 --memory=2G xo://\nOUTPUT --as=template %s\n", pool, network, template, base), "", nil, io.Discard)

	// The build context: the cidata files, the private key, a file to COPY.
	ctxDir := t.TempDir()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := gossh.MarshalPrivateKey(priv, "opendango-it")
	if err != nil {
		t.Fatal(err)
	}
	sshPub, _ := gossh.NewPublicKey(pub)
	name := fmt.Sprintf("opendango-it-packer-%d", time.Now().Unix())
	files := map[string]string{
		"key":              string(pem.EncodeToMemory(block)),
		"cidata/user-data": build.CloudConfig(user, strings.TrimSpace(string(gossh.MarshalAuthorizedKey(sshPub)))),
		"cidata/meta-data": fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", name, name),
		"hello.txt":        "hello from opendango\n",
	}
	for p, content := range files {
		os.MkdirAll(filepath.Join(ctxDir, filepath.Dir(p)), 0o755)
		if err := os.WriteFile(filepath.Join(ctxDir, p), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	src := fmt.Sprintf(`ARG XAPI_PASSWORD
FROM --remote_host=%s --remote_username=%s --remote_password=${XAPI_PASSWORD} --clone_template=%s --vm_name=%s --vm_memory=2048 --vcpus_max=2 --vcpus_atstartup=2 --firmware=bios --installtimeout=10m --network_names='[%q]' --cd_files='["cidata/user-data", "cidata/meta-data"]' --ssh_private_key_file=key --format=none --keep_vm=never plugin://xenserver-xva AS golden
CONNECT ssh --user=%s --timeout=15m
USER root
WORKDIR /opt/opendango
COPY --chmod=0640 hello.txt ./
RUN test "$(cat /opt/opendango/hello.txt)" = "hello from opendango"
RUN test "$(id -u)" = 0 && echo "built by $(hostname)"
`, host, username, base, name, network, user)

	var log bytes.Buffer
	run(ctx, t, src, ctxDir, map[string]string{"XAPI_PASSWORD": password}, io.MultiWriter(os.Stdout, &log))
	for _, want := range []string{"==> golden: Running the instructions", "built by " + name, "==> golden: Artifact:"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log lacks %q", want)
		}
	}
	if strings.Contains(log.String(), password) {
		t.Error("the XAPI password is in the log")
	}
}

func run(ctx context.Context, t *testing.T, src, ctxDir string, buildArgs map[string]string, out io.Writer) {
	t.Helper()
	res, err := parser.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build("Dangofile", res, buildArgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := build.Run(ctx, p, build.Options{ContextDir: ctxDir, Out: out, NoCache: true}); err != nil {
		t.Fatal(err)
	}
}

// deleteXOTemplate deletes the templates named name through the XO REST
// API.
func deleteXOTemplate(t *testing.T, name string) {
	t.Helper()
	client := &http.Client{}
	if os.Getenv("XOA_INSECURE") == "true" {
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	call := func(method, path string) (*http.Response, error) {
		req, err := http.NewRequest(method, strings.TrimSuffix(os.Getenv("XOA_ENDPOINT"), "/")+"/rest/v0"+path, nil)
		if err != nil {
			return nil, err
		}
		req.AddCookie(&http.Cookie{Name: "authenticationToken", Value: os.Getenv("XOA_TOKEN")})
		return client.Do(req)
	}
	resp, err := call(http.MethodGet, "/vm-templates?fields="+url.QueryEscape("id,name_label"))
	if err != nil {
		t.Errorf("listing the templates: %v", err)
		return
	}
	defer resp.Body.Close()
	var templates []struct {
		ID        string `json:"id"`
		NameLabel string `json:"name_label"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&templates); err != nil {
		t.Errorf("listing the templates: %v", err)
		return
	}
	for _, tmpl := range templates {
		if tmpl.NameLabel != name {
			continue
		}
		resp, err := call(http.MethodDelete, "/vm-templates/"+url.PathEscape(tmpl.ID))
		if err != nil || resp.StatusCode >= 300 {
			t.Errorf("deleting template %s: %v %v", tmpl.ID, err, resp)
			continue
		}
		resp.Body.Close()
	}
}
