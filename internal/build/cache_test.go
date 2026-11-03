package build

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sshcomm "github.com/littlejo/opendango/internal/communicator/ssh"
	"github.com/littlejo/opendango/internal/parser"
	"github.com/littlejo/opendango/internal/plan"
)

// keys returns the cache key of each stage of src, by name.
func keys(t *testing.T, src, ctxDir string, buildArgs map[string]string) map[string]string {
	t.Helper()
	res, err := parser.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build("Dangofile", res, buildArgs)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	byStage := map[*plan.Stage]string{}
	for _, s := range p.Stages {
		k, err := cacheKey(p.File, s, byStage[s.Parent], ctxDir)
		if err != nil {
			t.Fatal(err)
		}
		byStage[s] = k
		out[s.Name()] = k
	}
	return out
}

func TestCacheKey(t *testing.T) {
	ctxDir := t.TempDir()
	write := func(name, content string, mode os.FileMode) {
		p := filepath.Join(ctxDir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, mode)
	}
	write("site/index.html", "v1", 0o644)
	write("http/ks.cfg", "v1", 0o644)
	const src = `ARG T=debian
FROM --cpus=2 xo://${T} AS base
SERVE --port=8000 http
BOOT <esc>url=http://${DANGO_HTTP_ADDR}/ks.cfg<enter>
CONNECT ssh --user=debian
ARG V=1
ENV A=$V
RUN apt-get update
COPY site/ /var/www/

FROM base AS app
RUN make
OUTPUT --as=template app
`
	base := keys(t, src, ctxDir, nil)
	if len(base["base"]) != 64 || len(base["app"]) != 64 || base["base"] == base["app"] {
		t.Fatalf("keys = %v", base)
	}

	tests := []struct {
		name      string
		src       string
		buildArgs map[string]string
		change    func()
		// same lists the stages whose key must not change.
		same []string
	}{
		{name: "layout and comments", src: "# a comment\n\n" + strings.ReplaceAll(src, "RUN make", "\nrun   make"), same: []string{"base", "app"}},
		{name: "child instruction", src: strings.Replace(src, "RUN make", "RUN make install", 1), same: []string{"base"}},
		{name: "global ARG in FROM", src: src, buildArgs: map[string]string{"T": "ubuntu"}},
		{name: "stage ARG", src: src, buildArgs: map[string]string{"V": "2"}},
		{name: "FROM flag", src: strings.Replace(src, "--cpus=2", "--cpus=4", 1)},
		{name: "child FROM flag", src: strings.Replace(src, "FROM base", "FROM --memory=8G base", 1), same: []string{"base"}},
		{name: "CONNECT user", src: strings.Replace(src, "--user=debian", "--user=root", 1)},
		{name: "template name", src: strings.Replace(src, "--as=template app", "--as=template app2", 1), same: []string{"base"}},
		{name: "COPY content", src: src, change: func() { write("site/index.html", "v2", 0o644) }},
		{name: "COPY mode", src: src, change: func() { write("site/index.html", "v1", 0o600) }},
		{name: "COPY new file", src: src, change: func() { write("site/new.css", "x", 0o644) }},
		{name: "SERVE content", src: src, change: func() { write("http/ks.cfg", "v2", 0o644) }},
		{name: "SERVE port", src: strings.Replace(src, "--port=8000", "--port=8001", 1), same: []string{"base", "app"}},
		{name: "BOOT keys", src: strings.Replace(src, "<enter>", "<wait><enter>", 1)},
		{name: "SHELL", src: strings.Replace(src, "RUN apt-get update", "SHELL [\"/bin/bash\", \"-c\"]\nRUN apt-get update", 1)},
	}
	for _, tt := range tests {
		write("site/index.html", "v1", 0o644)
		write("http/ks.cfg", "v1", 0o644)
		os.Remove(filepath.Join(ctxDir, "site/new.css"))
		if tt.change != nil {
			tt.change()
		}
		got := keys(t, tt.src, ctxDir, tt.buildArgs)
		for _, stage := range []string{"base", "app"} {
			want := false
			for _, s := range tt.same {
				want = want || s == stage
			}
			if (got[stage] == base[stage]) != want {
				t.Errorf("%s: %s key unchanged = %v, want %v", tt.name, stage, got[stage] == base[stage], want)
			}
		}
	}
}

func TestCacheKeyBreakpoint(t *testing.T) {
	src := "FROM xo://t AS base\nCONNECT ssh --user=u\nBREAKPOINT\nFROM base AS app\nRUN x\nFROM xo://t AS other\n"
	got := keys(t, src, t.TempDir(), nil)
	if got["base"] != "" || got["app"] != "" || got["other"] == "" {
		t.Errorf("keys = %v: a BREAKPOINT stage and its children are not cached", got)
	}
}

func TestCacheKeyMissingSource(t *testing.T) {
	res, _ := parser.Parse(strings.NewReader("FROM xo://t\nCONNECT ssh --user=u\nCOPY nope /x\n"))
	p, err := plan.Build("Dangofile", res, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = cacheKey(p.File, p.Stages[0], "", t.TempDir())
	if err == nil || !strings.HasPrefix(err.Error(), "Dangofile:3: COPY: nope: no such file") {
		t.Errorf("err = %v", err)
	}
}

const cachedStages = `FROM xo://t AS base
CONNECT ssh --user=debian
USER root
WORKDIR /opt
ENV A=1
SHELL ["/bin/bash", "-c"]
RUN base-step

FROM base AS app
RUN app-step
OUTPUT --format=xva app.xva
OUTPUT --as=template app

FROM xo://t AS leaf
OUTPUT --format=xva leaf.xva
`

func TestCacheStore(t *testing.T) {
	sb := newStageBuilders()
	comm := &scriptComm{}
	err := Run(context.Background(), mustPlan(t, cachedStages), Options{NewBuilder: sb.factory(), Dial: dialTo(comm)})
	if err != nil {
		t.Fatal(err)
	}
	// base becomes a cache template, kept; app's template is tagged too.
	want := "vm-base:output:template(cache) vm-app:output:xva vm-app:output:template vm-leaf:output:xva vm-leaf:destroy"
	if got := strings.Join(sb.log, " "); got != want {
		t.Errorf("log = %q, want %q", got, want)
	}
	base, app, leaf := sb.byStage["base"], sb.byStage["app"], sb.byStage["leaf"]
	if len(base.tags) != 2 || !strings.HasPrefix(base.tags[0], CreatedTagPrefix) || base.tags[1] != CacheTagPrefix+base.lookups[0] {
		t.Errorf("base tags %v, lookups %v", base.tags, base.lookups)
	}
	if len(app.tags) != 1 || app.tags[0] != CacheTagPrefix+app.lookups[0] {
		t.Errorf("app tags %v, lookups %v", app.tags, app.lookups)
	}
	// leaf ends without a template: nothing to look up nor store.
	if len(leaf.lookups) != 0 || len(leaf.tags) != 0 {
		t.Errorf("leaf lookups %v, tags %v", leaf.lookups, leaf.tags)
	}
}

func TestCacheHit(t *testing.T) {
	// A first build fills the cache.
	first := newStageBuilders()
	if err := Run(context.Background(), mustPlan(t, cachedStages), Options{NewBuilder: first.factory(), Dial: dialTo(&scriptComm{})}); err != nil {
		t.Fatal(err)
	}
	cache := map[string]string{
		first.byStage["base"].lookups[0]: "tmpl-base",
		first.byStage["app"].lookups[0]:  "tmpl-app",
	}

	t.Run("every stage cached", func(t *testing.T) {
		sb := newStageBuilders()
		sb.cache = cache
		var log syncBuffer
		if err := Run(context.Background(), mustPlan(t, cachedStages), Options{NewBuilder: sb.factory(), Dial: dialTo(&scriptComm{}), Out: &log}); err != nil {
			t.Fatal(err)
		}
		// No VM for base and app: app's XVA comes from its cached template.
		for _, name := range []string{"base", "app"} {
			if got := strings.Join(sb.byStage[name].calls, " "); strings.Contains(got, "start") || strings.Contains(got, "destroy") {
				t.Errorf("%s calls %q", name, got)
			}
		}
		if got := strings.Join(sb.byStage["app"].calls, " "); got != "output:xva" {
			t.Errorf("app calls %q", got)
		}
		if !strings.Contains(log.String(), "==> base: Using the cached template tmpl-base (key ") {
			t.Errorf("log:\n%s", log.String())
		}
	})

	t.Run("child rebuilt from the cached parent", func(t *testing.T) {
		sb := newStageBuilders()
		sb.cache = map[string]string{first.byStage["base"].lookups[0]: "tmpl-base"}
		comm := &scriptComm{}
		src := strings.Replace(cachedStages, "RUN app-step", "RUN app-step-2", 1)
		if err := Run(context.Background(), mustPlan(t, src), Options{NewBuilder: sb.factory(), Dial: dialTo(comm)}); err != nil {
			t.Fatal(err)
		}
		if p := sb.byStage["app"].parent; p != "tmpl-base" {
			t.Errorf("app starts from %q", p)
		}
		// The replayed USER, SHELL, WORKDIR and ENV of base apply to app.
		var run string
		for _, c := range comm.commands {
			if strings.Contains(c, "app-step-2") {
				run = c
			}
		}
		for _, want := range []string{"sudo -n -H -u 'root'", "cd '\\''/opt'\\''", "export A='\\''1'\\''", "exec '\\''/bin/bash'\\'' '\\''-c'\\'' '\\''app-step-2'\\''"} {
			if !strings.Contains(run, want) {
				t.Errorf("RUN %q lacks %q", run, want)
			}
		}
	})

	t.Run("--no-cache", func(t *testing.T) {
		sb := newStageBuilders()
		sb.cache = cache
		if err := Run(context.Background(), mustPlan(t, cachedStages), Options{NoCache: true, NewBuilder: sb.factory(), Dial: dialTo(&scriptComm{})}); err != nil {
			t.Fatal(err)
		}
		for name, b := range sb.byStage {
			if len(b.lookups) != 0 || len(b.tags) != 0 {
				t.Errorf("%s lookups %v, tags %v", name, b.lookups, b.tags)
			}
		}
		if !strings.Contains(strings.Join(sb.log, " "), "vm-base:output:template(tmp)") {
			t.Errorf("log = %v", sb.log)
		}
	})
}

// TestCacheNotStoredAfterRetry checks that a stage fixed by hand during
// --on-error=ask is not cached.
func TestCacheNotStoredAfterRetry(t *testing.T) {
	fails := 1
	comm := &scriptComm{reply: func(cmd string) (string, error) {
		if strings.Contains(cmd, "base-step") && fails > 0 {
			fails--
			return "", &sshcomm.ExitError{Code: 1}
		}
		return "", nil
	}}
	sb := newStageBuilders()
	err := Run(context.Background(), mustPlan(t, cachedStages), Options{NewBuilder: sb.factory(), Dial: dialTo(comm), Ask: true, Stdin: strings.NewReader("r\n")})
	if err != nil {
		t.Fatal(err)
	}
	if b := sb.byStage["base"]; len(b.tags) != 0 {
		t.Errorf("base tags %v", b.tags)
	}
	// Not cached: the intermediate template is temporary.
	if got := strings.Join(sb.log, " "); !strings.Contains(got, "vm-base:output:template(tmp)") || !strings.HasSuffix(got, "vm-base:destroy") {
		t.Errorf("log = %q", got)
	}
}
