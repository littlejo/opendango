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
// The installation from an ISO (TestISOEndToEnd) also needs
// OPENDANGO_TEST_ISO, a Debian 12 DVD in an ISO SR of the pool
// (debian-12.*-amd64-DVD-1.iso), and Internet access from the VM network.
//
//   go test -tags integration -timeout 60m ./builders/xo/

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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
	// SERVE: the VM fetches a file from this machine.
	os.Mkdir(filepath.Join(ctxDir, "http"), 0o755)
	if err := os.WriteFile(filepath.Join(ctxDir, "http", "served.txt"), []byte("served by opendango\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf(`FROM %s AS it
SERVE http
CONNECT ssh --user=%s --timeout=15m
ENV --persist OPENDANGO_IT=1
USER root
WORKDIR /opt/opendango
COPY --chmod=0640 hello.txt ./
RUN test "$(cat /opt/opendango/hello.txt)" = "hello from opendango"
RUN url="http://$DANGO_HTTP_ADDR/served.txt"; test "$(curl -fsS "$url" || wget -qO- "$url")" = "served by opendango"
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
	if !strings.Contains(log.String(), "==> it: SERVE: GET /served.txt from ") {
		t.Error("the VM did not fetch the served file")
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
	ctx, client := run(t, src, build.Options{ContextDir: ctxDir, Out: io.MultiWriter(os.Stdout, &log), Jobs: 2, NoCache: true})
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

// TestCacheEndToEnd builds twice with the cache, then changes the child
// stage only: the parent comes from the cache, as an intermediate template
// that ListCache sees and DeleteCache removes.
func TestCacheEndToEnd(t *testing.T) {
	from, user, ctxDir := setup(t)
	start := time.Now().Add(-time.Minute)
	name := fmt.Sprintf("opendango-it-cache-%d", time.Now().Unix())
	src := fmt.Sprintf(`FROM %s AS base
CONNECT ssh --user=%s --timeout=15m
USER root
COPY hello.txt /opt/opendango/

FROM base AS app
RUN test -f /opt/opendango/hello.txt
OUTPUT --format=xva --compress=zstd out/app.xva
OUTPUT --as=template %s
`, from, user, name)

	client, err := xo.NewClientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	// Whatever happens, remove the templates and the cache entries made
	// here, and only those.
	t.Cleanup(func() {
		ctx := context.Background()
		for _, n := range []string{name, name + "-2"} {
			if id, err := xo.FindTemplate(ctx, client, n); err == nil {
				xo.DeleteTemplate(ctx, client, id)
			}
		}
		entries, _ := xo.ListCache(ctx, client)
		for _, e := range entries {
			if strings.HasPrefix(e.Name, "opendango-cache-base-") && e.Created.After(start) {
				xo.DeleteCache(ctx, client, e.ID)
			}
		}
	})
	buildOnce := func(src string) string {
		var log bytes.Buffer
		run(t, src, build.Options{ContextDir: ctxDir, Out: io.MultiWriter(os.Stdout, &log)})
		return log.String()
	}
	xva := filepath.Join(ctxDir, "out", "app.xva")

	t.Log("first build: fills the cache")
	buildOnce(src)

	t.Log("second build: everything from the cache, the XVA from the template")
	os.Remove(xva)
	log := buildOnce(src)
	for _, want := range []string{"==> base: Using the cached template", "==> app: Using the cached template", "==> app: Exporting XVA"} {
		if !strings.Contains(log, want) {
			t.Errorf("second build: log lacks %q", want)
		}
	}
	if strings.Contains(log, "Starting VM") {
		t.Error("second build started a VM")
	}
	if info, err := os.Stat(xva); err != nil || info.Size() == 0 {
		t.Errorf("XVA not exported from the cached template: %v", err)
	}

	t.Log("third build: the child changes, the parent comes from the cache")
	src2 := strings.Replace(src, "RUN test -f /opt/opendango/hello.txt", "RUN test -f /opt/opendango/hello.txt && test \"$(id -u)\" = 0", 1)
	src2 = strings.Replace(src2, "--as=template "+name, "--as=template "+name+"-2", 1)
	log = buildOnce(src2)
	if !strings.Contains(log, "==> base: Using the cached template") || !strings.Contains(log, "==> app: Starting VM") {
		t.Errorf("third build: base must come from the cache and app be rebuilt")
	}

	ctx := context.Background()
	entries, err := xo.ListCache(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	var mine []xo.CacheEntry
	for _, e := range entries {
		if strings.HasPrefix(e.Name, "opendango-cache-base-") && e.Created.After(start) {
			mine = append(mine, e)
		}
	}
	if len(mine) != 1 {
		t.Fatalf("cache entries of base: %+v, want 1", mine)
	}
	if err := xo.DeleteCache(ctx, client, mine[0].ID); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{name, name + "-2"} {
		deleteTemplate(ctx, t, client, n)
	}
}

// guestAgent is the Vates guest agent: XO only learns the IP address of the
// VM through it. Debian has no package for it.
const guestAgent = "https://gitlab.com/xen-project/xen-guest-agent/-/jobs/6041686362/artifacts/raw/target/release/xen-guest-agent_0.4.0_amd64.deb"

// preseed installs Debian 12 from the DVD without questions: no user (the
// cloud-init default user gets the key), a random root password (the
// installer requires one), cloud-init for the key and the guest agent for
// the IP address. The disk is the first one: the cloud-config drive is
// attached too.
const preseed = `d-i debian-installer/locale string en_US.UTF-8
d-i keyboard-configuration/xkb-keymap select us
d-i netcfg/choose_interface select auto
d-i netcfg/get_hostname string opendango
d-i netcfg/get_domain string
d-i passwd/make-user boolean false
d-i passwd/root-login boolean true
d-i passwd/root-password password ROOT_PASSWORD
d-i passwd/root-password-again password ROOT_PASSWORD
d-i clock-setup/utc boolean true
d-i time/zone string UTC
d-i clock-setup/ntp boolean false
d-i partman/early_command string debconf-set partman-auto/disk "$(list-devices disk | head -n1)"
d-i partman-auto/method string regular
d-i partman-efi/non_efi_system boolean true
d-i partman-auto/choose_recipe select atomic
d-i partman-partitioning/confirm_write_new_label boolean true
d-i partman/choose_partition select finish
d-i partman/confirm boolean true
d-i partman/confirm_nooverwrite boolean true
d-i apt-setup/cdrom/set-first boolean false
d-i apt-setup/cdrom/set-next boolean false
d-i apt-setup/cdrom/set-failed boolean false
d-i apt-setup/use_mirror boolean true
d-i mirror/country string manual
d-i mirror/http/hostname string deb.debian.org
d-i mirror/http/directory string /debian
d-i mirror/http/proxy string
tasksel tasksel/first multiselect standard, ssh-server
d-i pkgsel/include string cloud-init curl sudo
d-i pkgsel/upgrade select none
popularity-contest popularity-contest/participate boolean false
d-i grub-installer/only_debian boolean true
d-i grub-installer/bootdev string default
d-i preseed/late_command string in-target sh -c 'curl -fsSL -o /tmp/agent.deb ` + guestAgent + ` && dpkg -i /tmp/agent.deb && rm /tmp/agent.deb'
d-i cdrom-detect/eject boolean true
d-i finish-install/reboot_in_progress note
`

// TestISOEndToEnd installs Debian from its DVD: FROM --iso, SERVE for the
// preseed file, BOOT to type the installer command line at the GRUB prompt
// (the "Debian Bookworm 12" template boots with UEFI), then the usual
// connection through cloud-init. The template must keep neither the ISO
// nor the cloud-config drive.
func TestISOEndToEnd(t *testing.T) {
	env(t, "XOA_ENDPOINT")
	env(t, "XOA_TOKEN")
	iso := env(t, "OPENDANGO_TEST_ISO")
	from := fmt.Sprintf("--iso=%q --template=%q --cpus=2 --memory=2G --disk=10G", iso, "Debian Bookworm 12")
	if pool := os.Getenv("OPENDANGO_TEST_POOL"); pool != "" {
		from += fmt.Sprintf(" --pool=%q", pool)
	}
	if n := os.Getenv("OPENDANGO_TEST_NETWORK"); n != "" {
		from += fmt.Sprintf(" --network=%q", n)
	}
	ctxDir := t.TempDir()
	os.Mkdir(filepath.Join(ctxDir, "http"), 0o755)
	password := make([]byte, 16)
	rand.Read(password)
	answers := strings.ReplaceAll(preseed, "ROOT_PASSWORD", hex.EncodeToString(password))
	if err := os.WriteFile(filepath.Join(ctxDir, "http", "preseed.cfg"), []byte(answers), 0o600); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("opendango-it-iso-%d", time.Now().Unix())
	src := fmt.Sprintf(`FROM %s xo:// AS iso
SERVE http
BOOT <wait10>c<wait2>
BOOT linux /install.amd/vmlinuz auto=true priority=critical url=http://${DANGO_HTTP_ADDR}/preseed.cfg hostname=opendango domain= interface=auto --- quiet<enter><wait>
BOOT initrd /install.amd/initrd.gz<enter><wait>
BOOT boot<enter>
CONNECT ssh --user=debian --timeout=45m
USER root
RUN grep -q '^12\.' /etc/debian_version && echo "installed $(cat /etc/debian_version)"
OUTPUT --as=template %s
`, from, name)
	var log bytes.Buffer
	ctx, client := run(t, src, build.Options{ContextDir: ctxDir, Out: io.MultiWriter(os.Stdout, &log), NoCache: true})
	if !strings.Contains(log.String(), "==> iso: SERVE: GET /preseed.cfg from ") {
		t.Error("the installer did not fetch the preseed file")
	}
	id, err := xo.FindTemplate(ctx, client, name)
	if err != nil {
		t.Fatal(err)
	}
	disks, cds, err := xo.TemplateMedia(ctx, client, id)
	if err != nil {
		t.Error(err)
	} else if len(disks) != 1 || cds != 0 {
		t.Errorf("template disks %q, CDs with a disc %d: want one disk and no CD", disks, cds)
	}
	deleteTemplate(ctx, t, client, name)
}
