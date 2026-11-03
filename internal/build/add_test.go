package build

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAdd downloads URL sources from a local HTTP server and uploads them
// with the local shell, next to a local source.
func TestAdd(t *testing.T) {
	const tool = "#!/bin/sh\necho tool\n"
	srv := httptest.NewServer(http.FileServerFS(os.DirFS(writeTree(t, map[string]string{"files/tool.sh": tool}))))
	defer srv.Close()
	ctxDir := writeTree(t, map[string]string{"local.txt": "local"})
	sum := sha256.Sum256([]byte(tool))
	out := t.TempDir()
	src := fmt.Sprintf(`FROM xo://t
CONNECT ssh --user=tester
ARG SRV=%[2]s
ADD ${SRV}/files/tool.sh local.txt %[1]s/bin/
ADD --checksum=sha256:%[3]s --chmod=0755 ${SRV}/files/tool.sh %[1]s/tool
`, out, srv.URL, hex.EncodeToString(sum[:]))
	var log bytes.Buffer
	if err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: ctxDir, Out: &log, NewBuilder: only(&fakeBuilder{}), Dial: dialTo(newLocalComm(t)), NoCache: true}); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	for name, want := range map[string]struct {
		content string
		mode    os.FileMode
	}{
		// Downloads are 0600 unless --chmod, like in Docker.
		"bin/tool.sh":   {tool, 0o600},
		"bin/local.txt": {"local", 0o644},
		"tool":          {tool, 0o755},
	} {
		p := filepath.Join(out, name)
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := readFile(t, p); got != want.content || info.Mode().Perm() != want.mode {
			t.Errorf("%s = %q (mode %o), want %q (mode %o)", name, got, info.Mode().Perm(), want.content, want.mode)
		}
	}
	if !strings.Contains(log.String(), "==> stage-0: Downloading "+srv.URL+"/files/tool.sh\n") {
		t.Errorf("log:\n%s", log.String())
	}
}

func TestAddErrors(t *testing.T) {
	srv := httptest.NewServer(http.FileServerFS(os.DirFS(writeTree(t, map[string]string{"f": "content"}))))
	defer srv.Close()
	ctxDir := writeTree(t, map[string]string{"notes.txt": "x", "tree/x": "x"})
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write([]byte("x"))
	w.Close()
	os.WriteFile(filepath.Join(ctxDir, "site.tar.gz"), gz.Bytes(), 0o644)
	tar := make([]byte, 1024)
	copy(tar[257:], "ustar")
	os.WriteFile(filepath.Join(ctxDir, "site.tar"), tar, 0o644)

	zero := strings.Repeat("0", 64)
	tests := []struct{ add, err string }{
		{"--checksum=sha256:" + zero + " " + srv.URL + "/f /f", "checksum mismatch: got sha256:ed7002b439e9ac845f22357d822bac1444730fbdb6016d3ec9432297b9ec9f73, want sha256:" + zero},
		{srv.URL + "/missing /f", "404 Not Found"},
		{srv.URL + "/ /dir/", "no file name in the URL: set the destination file"},
		{"site.tar.gz /srv/", "site.tar.gz is an archive or a compressed file: ADD does not extract them yet"},
		{"notes.txt site.tar /srv/", "site.tar is an archive or a compressed file"},
	}
	for _, tt := range tests {
		src := "FROM xo://t\nCONNECT ssh --user=u\nADD " + tt.add + "\n"
		err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: ctxDir, NewBuilder: only(&fakeBuilder{}), Dial: dialTo(&scriptComm{}), NoCache: true})
		if err == nil || !strings.HasPrefix(err.Error(), "Dangofile:3: ADD: ") || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("ADD %s: err = %v, want %q", tt.add, err, tt.err)
		}
	}
	// A directory is copied, not checked: Docker does not extract inside.
	src := "FROM xo://t\nCONNECT ssh --user=u\nADD tree /srv/\n"
	if err := Run(context.Background(), mustPlan(t, src), Options{ContextDir: ctxDir, NewBuilder: only(&fakeBuilder{}), Dial: dialTo(&scriptComm{}), NoCache: true}); err != nil {
		t.Errorf("ADD tree: %v", err)
	}
}

// writeTree writes files (path: content) to a new temporary directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
