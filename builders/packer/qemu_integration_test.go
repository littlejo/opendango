//go:build integration

package packer_test

// End-to-end test of plugin://qemu (github.com/hashicorp/qemu), a builder of
// the table whose artifact is files: a stage starts from the disk of
// another, which goes to the images cache, and FETCH brings a file back.
//
// Required environment: the plugin, where the discovery finds it
// (`opendango plugin pull qemu`), qemu-system-x86_64 with KVM, xorriso (for
// the cloud-init seed), and OPENDANGO_TEST_QEMU_IMAGE, the path of a Debian
// cloud image (debian-12-genericcloud-amd64.qcow2). The CI runs it on a
// GitHub runner.

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
	"strings"
	"testing"
	"time"

	"github.com/littlejo/opendango/internal/build"
	"github.com/littlejo/opendango/internal/parser"
	"github.com/littlejo/opendango/internal/plan"
)

func TestQemuEndToEnd(t *testing.T) {
	image := env(t, "OPENDANGO_TEST_QEMU_IMAGE")
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	if _, _, err := build.FindPlugin(ctx, "qemu"); err != nil {
		t.Skipf("packer-plugin-qemu: %v", err)
	}
	for _, bin := range []string{"qemu-system-x86_64", "xorriso"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		t.Skipf("no KVM: %v", err)
	} else {
		f.Close()
	}

	ctxDir := t.TempDir()
	b := make([]byte, 12)
	rand.Read(b)
	password := hex.EncodeToString(b)
	// The cloud-init seed sets the password of the image's user.
	os.Mkdir(filepath.Join(ctxDir, "cidata"), 0o755)
	os.WriteFile(filepath.Join(ctxDir, "cidata", "meta-data"), []byte("instance-id: opendango-it\n"), 0o644)
	os.WriteFile(filepath.Join(ctxDir, "cidata", "user-data"), []byte(fmt.Sprintf(`#cloud-config
ssh_pwauth: true
chpasswd:
  expire: false
  users:
    - {name: debian, password: %s, type: text}
`, password)), 0o644)
	os.WriteFile(filepath.Join(ctxDir, "dango.yaml"), []byte(fmt.Sprintf(`qemu:
  dango:
    connect: ssh --user=debian --timeout=10m
  iso_url: %s
  iso_checksum: none
  disk_image: true
  format: qcow2
  headless: true
  accelerator: kvm
  memory: 1024
  cd_files: [cidata/user-data, cidata/meta-data]
  cd_label: cidata
  shutdown_command: sudo shutdown -P now
`, image)), 0o644)
	out := filepath.Join(ctxDir, "out-app")
	src := fmt.Sprintf(`ARG PASSWORD
FROM --ssh_password=${PASSWORD} plugin://qemu AS base
RUN echo base | sudo tee /etc/opendango-base

FROM --output_directory=%s base AS app
RUN test "$(cat /etc/opendango-base)" = base
RUN echo app | sudo tee /etc/opendango-app
FETCH /etc/opendango-base /etc/opendango-app fetched/
OUTPUT --format=checksum SHA256SUMS
`, out)
	images := t.TempDir()
	runQemu := func() string {
		var log bytes.Buffer
		res, err := parser.Parse(strings.NewReader(src))
		if err != nil {
			t.Fatal(err)
		}
		settings, err := plan.LoadSettings(filepath.Join(ctxDir, "dango.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		p, err := plan.BuildSettings("Dangofile", res, map[string]string{"PASSWORD": password}, settings)
		if err != nil {
			t.Fatal(err)
		}
		opts := build.Options{ContextDir: ctxDir, Out: io.MultiWriter(os.Stdout, &log), ImagesCache: images, FilesCache: t.TempDir()}
		if err := build.Run(ctx, p, opts); err != nil {
			t.Fatalf("%v", err)
		}
		return log.String()
	}

	runQemu()
	entries, err := build.ListImages(images)
	if err != nil || len(entries) != 1 {
		t.Fatalf("images cache: %+v, %v", entries, err)
	}
	for name, want := range map[string]string{"base": "base\n", "app": "app\n"} {
		if got, err := os.ReadFile(filepath.Join(ctxDir, "fetched", "opendango-"+name)); err != nil || string(got) != want {
			t.Errorf("fetched %s: %q, %v", name, got, err)
		}
	}
	if sums, err := os.ReadFile(filepath.Join(ctxDir, "SHA256SUMS")); err != nil || !strings.Contains(string(sums), "out-app/") {
		t.Errorf("SHA256SUMS: %q, %v", sums, err)
	}

	// The next build takes base from the cache.
	os.RemoveAll(out)
	if log := runQemu(); !strings.Contains(log, "==> base: Using the cached image "+entries[0].Dir) {
		t.Errorf("base was built again")
	}
}
