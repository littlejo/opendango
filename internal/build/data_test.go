package build

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/littlejo/opendango/builders/packer"
	"github.com/littlejo/opendango/internal/instructions"
	"github.com/littlejo/opendango/internal/parser"
	"github.com/littlejo/opendango/internal/plan"
)

// TestData runs a DATA with the test plugin's data source: its outputs
// are global ARGs, seen by FROM and by a stage that declares them.
func TestData(t *testing.T) {
	store := t.TempDir()
	t.Setenv("OPENDANGO_TEST_STORE", store)
	os.WriteFile(filepath.Join(store, "os=debian"), []byte("tmpl-42"), 0o644)
	ctxDir := t.TempDir()
	os.WriteFile(filepath.Join(ctxDir, "dango.yaml"), []byte("opendangotest-template:\n  same_pool_as: pool-a\n"), 0o644)
	settings, err := plan.LoadSettings(filepath.Join(ctxDir, "dango.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	src := `ARG OS=debian
DATA --tags="[\"os=${OS}\"]" plugin://opendangotest-template AS base
FROM --cpus=${base_count} xo://${base_id} AS a
ARG base_name
ARG base_tags
ARG base_count
`
	res, err := parser.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	find := testPlugin(t)
	findDS := func(ctx context.Context, source string) (*packer.Plugin, string, error) {
		pl, _, err := find(ctx, "opendangotest-local")
		return pl, strings.TrimPrefix(source, "opendangotest-"), err
	}
	var out bytes.Buffer
	p, err := plan.BuildData("Dangofile", res, map[string]string{"base_count": "7"}, settings, Data(context.Background(), ctxDir, &out, findDS))
	if err != nil {
		t.Fatal(err)
	}
	s := p.Stages[0]
	if s.From.Source != "tmpl-42" || s.From.Flags["cpus"] != "7" {
		t.Errorf("FROM %+v", s.From)
	}
	want := map[string]string{"base_name": "lookup from pool-a", "base_tags": `["os=debian"]`, "base_count": "7"}
	for i, name := range []string{"base_name", "base_tags", "base_count"} {
		if a := s.Steps[i].(*instructions.Arg); a.Name != name || a.Value != want[name] {
			t.Errorf("step %d: %s = %q, want %s = %q", i, a.Name, a.Value, name, want[name])
		}
	}
	if got := out.String(); got != "==> DATA plugin://opendangotest-template AS base\n    base_count base_id base_name base_tags\n" {
		t.Errorf("log = %q", got)
	}

	// A failing data source fails the plan at its line.
	res, _ = parser.Parse(strings.NewReader("DATA --tags='[\"missing\"]' plugin://opendangotest-template AS base\nFROM xo://t\n"))
	_, err = plan.BuildData("Dangofile", res, nil, nil, Data(context.Background(), ctxDir, &out, findDS))
	if err == nil || !strings.Contains(err.Error(), "Dangofile:1: DATA plugin://opendangotest-template: ") || !strings.Contains(err.Error(), "no artifact with the tags") {
		t.Errorf("error = %v", err)
	}
}
