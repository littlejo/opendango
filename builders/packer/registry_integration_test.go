//go:build integration

package packer

import (
	"context"
	"testing"
)

// TestPluginPullEndToEnd installs real plugins from releases.hashicorp.com
// (official) and from GitHub releases, and runs their describe. The search
// is not used: the GitHub search API allows 10 requests a minute without a
// token.
func TestPluginPullEndToEnd(t *testing.T) {
	reg, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	reg.Dir = t.TempDir()
	ctx := context.Background()

	src, err := reg.Resolve(ctx, "qemu")
	if err != nil || src.String() != "github.com/hashicorp/qemu" {
		t.Fatalf("Resolve(qemu) = %v, %v", src, err)
	}
	for _, tt := range []struct {
		src     Source
		version string
	}{
		{src, ""},
		{Source{"vatesfr", "xenserver"}, "0.11.4"},
	} {
		pl, installed, err := reg.Install(ctx, tt.src, tt.version)
		if err != nil || !installed {
			t.Fatalf("Install(%s@%s) = %v, %v", tt.src, tt.version, installed, err)
		}
		d, err := pl.Describe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s %s: %s, builders %v", tt.src, pl.Version, pl.Path, d.Builders)
		if found, _, err := Find(Discover([]string{reg.Dir}, nil), tt.src.Name); err != nil || found.Path != pl.Path {
			t.Errorf("Find(%s) = %+v, %v", tt.src.Name, found, err)
		}
	}
}
