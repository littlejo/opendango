package xo

import (
	"context"
	"slices"
	"testing"
)

func TestLocalAddr(t *testing.T) {
	tests := []struct{ base, want string }{
		{"https://127.0.0.1", "127.0.0.1"},
		{"http://127.0.0.1:8080/", "127.0.0.1"},
		{"https://[::1]", "::1"},
	}
	for _, tt := range tests {
		b := NewBuilder(Config{}, &Client{BaseURL: tt.base}, "vm", nil)
		got, err := b.LocalAddr(context.Background())
		if err != nil || got != tt.want {
			t.Errorf("%s: LocalAddr = %q, %v; want %q", tt.base, got, err, tt.want)
		}
	}
}

func TestReplaced(t *testing.T) {
	inherited := []string{"team=infra", "team", "teams=x", "vendor=acme", "team=web"}
	if got := replaced(inherited, "team=web"); !slices.Equal(got, []string{"team=infra"}) {
		t.Errorf("replaced = %q", got)
	}
	// Not a label: nothing replaced.
	if got := replaced(inherited, cacheTag+"k"); got != nil {
		t.Errorf("replaced = %q", got)
	}
}

func TestIsCacheTag(t *testing.T) {
	for tag, want := range map[string]bool{cacheTag + "k": true, createdTag + "1": true, "team=web": false, "opendango-tmp": false} {
		if got := isCacheTag(tag); got != want {
			t.Errorf("isCacheTag(%q) = %v", tag, got)
		}
	}
}
