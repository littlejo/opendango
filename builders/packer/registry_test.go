package packer

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestParseRef(t *testing.T) {
	for _, tt := range []struct {
		ref, name, src, version, err string
	}{
		{"qemu", "qemu", "", "", ""},
		{"qemu@1.1.8", "qemu", "", "1.1.8", ""},
		{"qemu@v1.1.8", "qemu", "", "1.1.8", ""},
		{"github.com/vatesfr/xenserver", "xenserver", "github.com/vatesfr/xenserver", "", ""},
		{"github.com/vatesfr/xenserver@0.11.4-rc.1", "xenserver", "github.com/vatesfr/xenserver", "0.11.4-rc.1", ""},
		{"Qemu", "", "", "", "invalid plugin name"},
		{"packer_qemu", "", "", "", "invalid plugin name"},
		{"qemu@latest", "", "", "", "invalid plugin version"},
		{"qemu@", "", "", "", "invalid plugin version"},
		{"gitlab.com/a/b", "", "", "", "expected github.com/<owner>/<name>"},
		{"github.com/a/b/c", "", "", "", "expected github.com/<owner>/<name>"},
		{"github.com/../b", "", "", "", "expected github.com/<owner>/<name>"},
	} {
		name, src, version, err := ParseRef(tt.ref)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%s: err = %v, want %q", tt.ref, err, tt.err)
			}
			continue
		}
		got := ""
		if src != nil {
			got = src.String()
		}
		if err != nil || name != tt.name || got != tt.src || version != tt.version {
			t.Errorf("%s: got %q %q %q %v", tt.ref, name, got, version, err)
		}
	}
}

// fakeRegistry serves the HashiCorp release API, the GitHub API and the
// downloads, with the test plugin as the binary of every release.
type fakeRegistry struct {
	*httptest.Server
	mu   sync.Mutex
	auth map[string]string
}

func newFakeRegistry(t *testing.T) (*fakeRegistry, *Registry) {
	t.Helper()
	bin, err := os.ReadFile(testPlugin(t))
	if err != nil {
		t.Fatal(err)
	}
	platform := runtime.GOOS + "_" + runtime.GOARCH
	archive := func(name, version string) []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create(fmt.Sprintf("packer-plugin-%s_v%s_x5.0_%s", name, version, platform))
		w.Write(bin)
		w, _ = zw.Create("LICENSE.txt")
		w.Write([]byte("license"))
		zw.Close()
		return buf.Bytes()
	}
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	files := map[string][]byte{}
	f := &fakeRegistry{auth: map[string]string{}}
	mux := http.NewServeMux()
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	reply := func(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }

	// HashiCorp: opendangotest 1.2.3.
	official := archive("opendangotest", "1.2.3")
	files["/dl/packer-plugin-opendangotest_1.2.3_"+platform+".zip"] = official
	files["/dl/hc_SHA256SUMS"] = []byte(sum(official) + "  packer-plugin-opendangotest_1.2.3_" + platform + ".zip\n")
	mux.HandleFunc("/hc/v1/products", func(w http.ResponseWriter, r *http.Request) {
		reply(w, []string{"packer", "packer-plugin-opendangotest", "vault"})
	})
	mux.HandleFunc("/hc/v1/releases/packer-plugin-opendangotest/{v}", func(w http.ResponseWriter, r *http.Request) {
		if v := r.PathValue("v"); v != "latest" && v != "1.2.3" {
			http.NotFound(w, r)
			return
		}
		reply(w, map[string]any{
			"version":     "1.2.3",
			"url_shasums": f.URL + "/dl/hc_SHA256SUMS",
			"builds": []map[string]string{
				{"os": "plan9", "arch": "mips", "url": f.URL + "/dl/nope.zip"},
				{"os": runtime.GOOS, "arch": runtime.GOARCH, "url": f.URL + "/dl/packer-plugin-opendangotest_1.2.3_" + platform + ".zip"},
			},
		})
	})

	// GitHub.
	type repo struct {
		Name        string         `json:"name"`
		Owner       map[string]any `json:"owner"`
		Description string         `json:"description"`
		Stars       int            `json:"stargazers_count"`
		Fork        bool           `json:"fork"`
		Archived    bool           `json:"archived"`
	}
	mk := func(owner, name string, stars int, fork, archived bool) repo {
		return repo{name, map[string]any{"login": owner}, owner + "'s " + name, stars, fork, archived}
	}
	mux.HandleFunc("/gh/search/repositories", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		q := r.URL.Query().Get("q")
		if strings.HasPrefix(q, "packer-plugin ") != strings.HasSuffix(q, " fork:true in:name") {
			http.Error(w, "forks: "+q, http.StatusBadRequest)
			return
		}
		var items []repo
		switch {
		case strings.Contains(q, "limited"):
			w.Header().Set("X-RateLimit-Remaining", "0")
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
			return
		case strings.Contains(q, "thing"):
			items = []repo{
				mk("other", "packer-plugin-thing", 50, true, false),
				mk("copy", "packer-plugin-thing", 0, true, false),
				mk("acme", "packer-plugin-thing", 5, false, false),
				mk("old", "packer-plugin-thingy", 1, false, true),
				mk("acme", "packer-plugin-thing2", 9, false, false),
				mk("acme", "thing-tools", 90, false, false),
			}
		case strings.Contains(q, "dup"):
			items = []repo{mk("acme", "packer-plugin-dup", 1, false, false), mk("beta", "Packer-Plugin-Dup", 2, false, false)}
		case strings.Contains(q, "opendango"):
			items = []repo{mk("hashicorp", "packer-plugin-opendangotest", 7, false, false)}
		}
		reply(w, map[string]any{"items": items})
	})
	thing := archive("thing", "2.0.0")
	zipName := "packer-plugin-thing_v2.0.0_x5.0_" + platform + ".zip"
	files["/dl/"+zipName] = thing
	files["/dl/thing_SHA256SUMS"] = []byte(sum([]byte("other")) + "  other.zip\n" + sum(thing) + "  " + zipName + "\n")
	files["/dl/broken.zip"] = thing
	files["/dl/broken_SHA256SUMS"] = []byte(sum([]byte("other")) + "  packer-plugin-broken_v1.0.0_x5.0_" + platform + ".zip\n")
	mux.HandleFunc("/gh/repos/acme/{repo}/releases/{which...}", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		switch r.PathValue("repo") + " " + r.PathValue("which") {
		case "packer-plugin-thing latest", "packer-plugin-thing tags/v2.0.0":
			reply(w, map[string]any{"tag_name": "v2.0.0", "assets": []map[string]string{
				{"name": "packer-plugin-thing_v2.0.0_x5.0_plan9_mips.zip", "browser_download_url": f.URL + "/dl/nope.zip"},
				{"name": "packer-plugin-thing_v2.0.0_x6.0_" + platform + ".zip", "browser_download_url": f.URL + "/dl/nope.zip"},
				{"name": zipName, "browser_download_url": f.URL + "/dl/" + zipName},
				{"name": "packer-plugin-thing_v2.0.0_SHA256SUMS", "browser_download_url": f.URL + "/dl/thing_SHA256SUMS"},
			}})
		case "packer-plugin-broken latest":
			reply(w, map[string]any{"tag_name": "v1.0.0", "assets": []map[string]string{
				{"name": "packer-plugin-broken_v1.0.0_x5.0_" + platform + ".zip", "browser_download_url": f.URL + "/dl/broken.zip"},
				{"name": "packer-plugin-broken_v1.0.0_SHA256SUMS", "browser_download_url": f.URL + "/dl/broken_SHA256SUMS"},
			}})
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	})
	mux.HandleFunc("/dl/{file}", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	})

	return f, &Registry{
		HashiCorpAPI: f.URL + "/hc",
		GitHubAPI:    f.URL + "/gh",
		Token:        "secret-token",
		Client:       f.Client(),
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Dir:          t.TempDir(),
	}
}

func (f *fakeRegistry) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth[r.URL.Path] = r.Header.Get("Authorization")
}

func TestInstall(t *testing.T) {
	f, reg := newFakeRegistry(t)
	ctx := context.Background()
	platform := runtime.GOOS + "_" + runtime.GOARCH

	for _, tt := range []struct {
		name, version, src, path string
	}{
		// Official first.
		{"opendangotest", "", "github.com/hashicorp/opendangotest", "github.com/hashicorp/opendangotest/packer-plugin-opendangotest_v1.2.3_x5.0_" + platform},
		// Forks, archived repositories and other names are ignored.
		{"thing", "2.0.0", "github.com/acme/thing", "github.com/acme/thing/packer-plugin-thing_v2.0.0_x5.0_" + platform},
	} {
		src, err := reg.Resolve(ctx, tt.name)
		if err != nil || src.String() != tt.src {
			t.Fatalf("Resolve(%s) = %v, %v", tt.name, src, err)
		}
		pl, installed, err := reg.Install(ctx, src, tt.version)
		if err != nil || !installed {
			t.Fatalf("Install(%s) = %v, %v", src, installed, err)
		}
		want := filepath.Join(reg.Dir, tt.path)
		if pl.Path != want || pl.Name != tt.name {
			t.Errorf("Install(%s) = %+v, want %s", src, pl, want)
		}
		info, err := os.Stat(want)
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Fatalf("%s: %v %v", want, info, err)
		}
		bin, _ := os.ReadFile(want)
		h := sha256.Sum256(bin)
		if got := readFile(t, want+"_SHA256SUM"); got != hex.EncodeToString(h[:]) {
			t.Errorf("_SHA256SUM = %q", got)
		}
		// Found by the discovery of plugin:// stages.
		found, _, err := Find(Discover([]string{reg.Dir}, nil), tt.name)
		if err != nil || found.Path != want {
			t.Errorf("Find(%s) = %+v, %v", tt.name, found, err)
		}
		// Kept as is the second time.
		if _, installed, err := reg.Install(ctx, src, ""); err != nil || installed {
			t.Errorf("second Install(%s) = %v, %v", src, installed, err)
		}
	}
	// Only the temporary files are gone.
	entries, _ := os.ReadDir(filepath.Join(reg.Dir, "github.com/acme/thing"))
	if len(entries) != 2 {
		t.Errorf("files left: %v", entries)
	}
	// The token goes to the GitHub API only.
	for p, auth := range f.auth {
		want := ""
		if strings.HasPrefix(p, "/gh/") {
			want = "Bearer secret-token"
		}
		if auth != want {
			t.Errorf("%s: Authorization %q, want %q", p, auth, want)
		}
	}
}

func TestInstallErrors(t *testing.T) {
	_, reg := newFakeRegistry(t)
	ctx := context.Background()

	for _, tt := range []struct {
		name, err string
	}{
		{"nothing", `no plugin named "nothing": look for one with ` + "`opendango plugin search`"},
		{"dup", `several plugins are named "dup", give the source: github.com/acme/dup, github.com/beta/dup`},
		{"limited", "github: API rate limit exceeded, wait a minute"},
	} {
		if _, err := reg.Resolve(ctx, tt.name); err == nil || err.Error() != tt.err {
			t.Errorf("Resolve(%s): err = %v, want %q", tt.name, err, tt.err)
		}
	}

	for _, tt := range []struct {
		src     Source
		version string
		err     string
	}{
		{Source{"hashicorp", "opendangotest"}, "9.9.9", "github.com/hashicorp/opendangotest has no version 9.9.9"},
		{Source{"acme", "thing"}, "3.0.0", "github.com/acme/thing has no release v3.0.0 (or no repository acme/packer-plugin-thing)"},
		{Source{"acme", "nope"}, "", "github.com/acme/nope has no release (or no repository acme/packer-plugin-nope)"},
		{Source{"acme", "broken"}, "", "sha256 "},
	} {
		if _, _, err := reg.Install(ctx, tt.src, tt.version); err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("Install(%s@%s): err = %v, want %q", tt.src, tt.version, err, tt.err)
		}
	}
	// Nothing is left of the broken download.
	if entries, _ := os.ReadDir(filepath.Join(reg.Dir, "github.com/acme/broken")); len(entries) != 0 {
		t.Errorf("files left: %v", entries)
	}

	reg.OS, reg.Arch = "plan9", "386"
	for _, src := range []Source{{"hashicorp", "opendangotest"}, {"acme", "thing"}} {
		if _, _, err := reg.Install(ctx, src, ""); err == nil || !strings.Contains(err.Error(), "has no build for plan9/386") {
			t.Errorf("Install(%s) on plan9: err = %v", src, err)
		}
	}

	reg.Token = ""
	if _, err := reg.Resolve(ctx, "limited"); err == nil || !strings.Contains(err.Error(), "set GITHUB_TOKEN") {
		t.Errorf("rate limit without token: err = %v", err)
	}
}

func TestSearch(t *testing.T) {
	_, reg := newFakeRegistry(t)
	ctx := context.Background()
	for _, tt := range []struct {
		term string
		want []SearchResult
		err  string
	}{
		{"thing", []SearchResult{
			{Source: Source{"other", "thing"}, Fork: true, Description: "other's packer-plugin-thing", Stars: 50},
			{Source: Source{"acme", "thing2"}, Description: "acme's packer-plugin-thing2", Stars: 9},
			{Source: Source{"acme", "thing"}, Description: "acme's packer-plugin-thing", Stars: 5},
		}, ""},
		{"OpenDango", []SearchResult{
			{Source: Source{"hashicorp", "opendangotest"}, Official: true, Description: "hashicorp's packer-plugin-opendangotest", Stars: 7},
		}, ""},
		{"nothing", nil, ""},
		// The official results come with the GitHub error.
		{"limited", nil, "rate limit"},
	} {
		got, err := reg.Search(ctx, tt.term)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("Search(%s): err = %v", tt.term, err)
			}
		} else if err != nil {
			t.Errorf("Search(%s): %v", tt.term, err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Search(%s) = %+v, want %+v", tt.term, got, tt.want)
		}
	}
}

func TestRemove(t *testing.T) {
	reg := &Registry{Dir: t.TempDir()}
	arch := "_x5.0_" + runtime.GOOS + "_" + runtime.GOARCH
	for _, f := range []string{
		"github.com/hashicorp/qemu/packer-plugin-qemu_v1.1.7" + arch,
		"github.com/hashicorp/qemu/packer-plugin-qemu_v1.1.8" + arch,
		"github.com/acme/dup/packer-plugin-dup_v1.0.0" + arch,
		"github.com/beta/dup/packer-plugin-dup_v1.0.0" + arch,
	} {
		p := filepath.Join(reg.Dir, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, nil, 0o755)
		os.WriteFile(p+"_SHA256SUM", nil, 0o644)
	}
	rel := func(paths []string) []string {
		var out []string
		for _, p := range paths {
			r, _ := filepath.Rel(reg.Dir, p)
			out = append(out, r)
		}
		return out
	}

	got, err := reg.Remove("qemu", nil, "1.1.7")
	if want := []string{"github.com/hashicorp/qemu/packer-plugin-qemu_v1.1.7" + arch}; err != nil || !reflect.DeepEqual(rel(got), want) {
		t.Errorf("Remove(qemu@1.1.7) = %v, %v", rel(got), err)
	}
	if _, err := os.Stat(filepath.Join(reg.Dir, "github.com/hashicorp/qemu/packer-plugin-qemu_v1.1.7"+arch+"_SHA256SUM")); !os.IsNotExist(err) {
		t.Errorf("the _SHA256SUM file is left: %v", err)
	}
	if _, err := reg.Remove("qemu", nil, "1.1.7"); err == nil || !strings.Contains(err.Error(), "plugin qemu@1.1.7 is not installed") {
		t.Errorf("Remove(qemu@1.1.7) again: err = %v", err)
	}
	if _, err := reg.Remove("dup", nil, ""); err == nil || !strings.Contains(err.Error(), "give the source: github.com/acme/dup, github.com/beta/dup") {
		t.Errorf("Remove(dup): err = %v", err)
	}
	if _, err := reg.Remove("dup", &Source{"beta", "dup"}, ""); err != nil {
		t.Errorf("Remove(github.com/beta/dup): %v", err)
	}
	if _, err := reg.Remove("qemu", &Source{"hashicorp", "qemu"}, ""); err != nil {
		t.Errorf("Remove(github.com/hashicorp/qemu): %v", err)
	}
	// The empty directories go too.
	entries, _ := os.ReadDir(filepath.Join(reg.Dir, "github.com"))
	if len(entries) != 1 || entries[0].Name() != "acme" {
		t.Errorf("left: %v", entries)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
