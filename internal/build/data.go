package build

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/littlejo/opendango/builders/packer"
	"github.com/littlejo/opendango/internal/instructions"
	"github.com/littlejo/opendango/internal/plan"
)

// FindDatasource looks for the plugin of a data source type (amazon-ami,
// http) like FindPlugin, and checks that it has it.
func FindDatasource(ctx context.Context, source string) (*packer.Plugin, string, error) {
	plugins := packer.Discover(packer.SearchDirs(), filepath.SplitList(os.Getenv("PATH")))
	pl, name, err := packer.Find(plugins, source)
	if err != nil {
		return nil, "", err
	}
	d, err := pl.Describe(ctx)
	if err != nil {
		return nil, "", err
	}
	if !slices.Contains(d.Datasources, name) {
		return nil, "", fmt.Errorf("plugin: %s has no data source %q (data sources: %s)", pl.Path, name, strings.Join(d.Datasources, ", "))
	}
	return pl, name, nil
}

// Data returns the plan.DataFunc that runs the DATA instructions with the
// plugins' data sources (found by find, FindDatasource when nil), in the
// Dangofile directory. It prints the names of the variables, not their
// values (a data source can read secrets).
func Data(ctx context.Context, contextDir string, out io.Writer, find FindPluginFunc) plan.DataFunc {
	if find == nil {
		find = FindDatasource
	}
	return func(d *instructions.Data) (map[string]string, error) {
		cfg, err := packer.ParseConfig(d.Source, d.Settings)
		if err != nil {
			return nil, err
		}
		pl, name, err := find(ctx, cfg.Component)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(out, "==> DATA plugin://%s AS %s\n", cfg.Component, d.Name)
		outputs, err := packer.Datasource(pl, name, contextDir, cfg.Settings)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(outputs))
		for k := range outputs {
			names = append(names, instructions.DataVar(d.Name, k))
		}
		sort.Strings(names)
		fmt.Fprintf(out, "    %s\n", strings.Join(names, " "))
		return outputs, nil
	}
}
