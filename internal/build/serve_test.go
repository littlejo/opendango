package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/littlejo/opendango/internal/instructions"
	"github.com/littlejo/opendango/internal/plan"
)

// TestServe serves a directory to RUN instructions run with the local shell,
// which fetch it with curl at ${DANGO_HTTP_ADDR}.
func TestServe(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
	ctxDir := t.TempDir()
	os.MkdirAll(filepath.Join(ctxDir, "http", "sub"), 0o755)
	os.WriteFile(filepath.Join(ctxDir, "http", "sub", "ks.cfg"), []byte("kickstart"), 0o644)
	os.WriteFile(filepath.Join(ctxDir, "secret"), []byte("secret"), 0o644)
	// A link out of the directory is not followed.
	os.Symlink("../secret", filepath.Join(ctxDir, "http", "escape"))
	out := t.TempDir()
	src := fmt.Sprintf(`FROM xo://t
SERVE http
CONNECT ssh --user=tester
USER other
RUN curl -fsS "http://$DANGO_HTTP_ADDR/sub/ks.cfg" > %[1]s/ks.cfg
RUN curl -fsS -o /dev/null "http://$DANGO_HTTP_ADDR/escape" 2>/dev/null; echo $? > %[1]s/escape
`, out)
	var log bytes.Buffer
	err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: ctxDir, Out: &log, NewBuilder: only(&fakeBuilder{}), Dial: dialTo(newLocalComm(t)), NoCache: true})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if got := readFile(t, filepath.Join(out, "ks.cfg")); got != "kickstart" {
		t.Errorf("ks.cfg = %q", got)
	}
	// curl -f exits with 22 on an HTTP error.
	if got := readFile(t, filepath.Join(out, "escape")); got != "22\n" {
		t.Errorf("escape: curl exit code %q, want 22", got)
	}
	m := regexp.MustCompile(`==> stage-0: Serving http at http://(127\.0\.0\.1:\d+)/\n`).FindStringSubmatch(log.String())
	if m == nil {
		t.Fatalf("no server address in:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "==> stage-0: SERVE: GET /sub/ks.cfg from 127.0.0.1:") {
		t.Errorf("request not logged:\n%s", log.String())
	}
	// Stopped with the stage.
	if conn, err := net.Dial("tcp", m[1]); err == nil {
		conn.Close()
		t.Errorf("the server still listens on %s", m[1])
	}
}

func TestServeOptions(t *testing.T) {
	ctxDir := t.TempDir()
	os.Mkdir(filepath.Join(ctxDir, "http"), 0o755)

	t.Run("addr", func(t *testing.T) {
		comm := &scriptComm{}
		var log bytes.Buffer
		src := "FROM xo://t\nSERVE --addr=192.0.2.1 http\nCONNECT ssh --user=u\nRUN true\n"
		if err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: ctxDir, Out: &log, NewBuilder: only(&fakeBuilder{}), Dial: dialTo(comm), NoCache: true}); err != nil {
			t.Fatalf("%v\n%s", err, log.String())
		}
		m := regexp.MustCompile(`Serving http at http://(192\.0\.2\.1:\d+)/`).FindStringSubmatch(log.String())
		if m == nil {
			t.Fatalf("log:\n%s", log.String())
		}
		want := "/bin/sh -c " + instructions.ShellQuote("export DANGO_HTTP_ADDR="+instructions.ShellQuote(m[1])+"\ntrue")
		if !contains(comm.commands, want) {
			t.Errorf("DANGO_HTTP_ADDR not exported: %q", comm.commands)
		}
	})

	t.Run("not a directory", func(t *testing.T) {
		src := "FROM xo://t\nSERVE nope\nCONNECT ssh --user=u\nRUN true\n"
		b := &fakeBuilder{}
		err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: ctxDir, NewBuilder: only(b), Dial: dialTo(&scriptComm{}), NoCache: true})
		if err == nil || err.Error() != "Dangofile:2: SERVE: nope is not a directory" || len(b.calls) != 0 {
			t.Errorf("err = %v, calls = %v", err, b.calls)
		}
	})

	t.Run("cache hit", func(t *testing.T) {
		src := "FROM xo://t\nSERVE http\nCONNECT ssh --user=u\nRUN true\nOUTPUT --as=template golden\n"
		p := mustPlan(t, src)
		key, err := cacheKey(p.File, p.Stages[0], "", ctxDir)
		if err != nil {
			t.Fatal(err)
		}
		var log bytes.Buffer
		b := &fakeBuilder{cache: map[string]string{key: "tpl"}}
		if err := Run(context.Background(), p, Options{ContextDir: ctxDir, Out: &log, NewBuilder: only(b)}); err != nil {
			t.Fatalf("%v\n%s", err, log.String())
		}
		// No VM: no server.
		if strings.Contains(log.String(), "Serving") {
			t.Errorf("log:\n%s", log.String())
		}
	})
}

func TestServeDir(t *testing.T) {
	wd, _ := os.Getwd()
	got, err := serveDir(".", &instructions.Serve{Dir: "."})
	if err != nil || got != wd {
		t.Errorf("serveDir = %q, %v; want %q", got, err, wd)
	}
}

// TestBoot checks that BOOT types its keys once the VM has started, before
// the connection, with the SERVE address.
func TestBoot(t *testing.T) {
	ctxDir := t.TempDir()
	os.Mkdir(filepath.Join(ctxDir, "http"), 0o755)
	src := "FROM xo://t\nSERVE --addr=192.0.2.1 http\nBOOT <esc>url=http://${DANGO_HTTP_ADDR}/ks.cfg<enter>\nBOOT <wait5>\nCONNECT ssh --user=u\nRUN true\n"
	b := &fakeBuilder{}
	var log bytes.Buffer
	if err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: ctxDir, Out: &log, NewBuilder: only(b), Dial: dialTo(&scriptComm{}), NoCache: true}); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	addr := regexp.MustCompile(`Serving http at http://(192\.0\.2\.1:\d+)/`).FindStringSubmatch(log.String())
	if addr == nil {
		t.Fatalf("log:\n%s", log.String())
	}
	want := []string{"start", "keys:<esc>url=http://" + addr[1] + "/ks.cfg<enter>", "keys:<wait5>", "address"}
	if len(b.calls) < 4 || !reflect.DeepEqual(b.calls[:4], want) {
		t.Errorf("calls = %q, want %q first", b.calls, want)
	}
	if !strings.Contains(log.String(), "==> stage-0: BOOT <esc>url=http://"+addr[1]+"/ks.cfg<enter>\n") {
		t.Errorf("log:\n%s", log.String())
	}
}

// TestBootFailure checks that a failed BOOT stops the build and destroys
// the VM.
func TestBootFailure(t *testing.T) {
	b := &failingKeys{}
	err := Run(context.Background(), mustPlan(t, "FROM xo://t\nBOOT <enter>\n"), Options{NewBuilder: func(*plan.Stage, string) (Builder, error) { return b, nil }, NoCache: true})
	if err == nil || err.Error() != "Dangofile:2: BOOT: console closed" || !contains(b.calls, "destroy") {
		t.Errorf("err = %v, calls = %q", err, b.calls)
	}
}

type failingKeys struct{ fakeBuilder }

func (b *failingKeys) SendKeys(context.Context, string) error { return errors.New("console closed") }
