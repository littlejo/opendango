package xo

import (
	"testing"
	"time"
)

func TestCached(t *testing.T) {
	objs := []object{
		{ID: "other-pool", Pool: "p2", Tags: []string{cacheTag + "k"}},
		{ID: "untagged", Pool: "p1"},
		{ID: "hit", Pool: "p1", Tags: []string{"x", cacheTag + "k"}},
	}
	if got := cached(objs, "p1", "k"); got != "hit" {
		t.Errorf("cached = %q, want hit", got)
	}
	if got := cached(objs, "p1", "other"); got != "" {
		t.Errorf("cached = %q, want none", got)
	}
}

func TestCacheEntries(t *testing.T) {
	objs := []object{
		{ID: "output", NameLabel: "golden", Tags: []string{cacheTag + "k1"}},
		{ID: "new", NameLabel: "opendango-cache-b-200", Tags: []string{cacheTag + "k2", createdTag + "200"}},
		{ID: "old", NameLabel: "opendango-cache-a-100", Tags: []string{createdTag + "100", cacheTag + "k3"}},
		{ID: "bad", Tags: []string{createdTag + "soon"}},
	}
	got := cacheEntries(objs)
	// The OUTPUT template is never listed; oldest first.
	if len(got) != 2 || got[0].ID != "old" || got[1].ID != "new" || !got[0].Created.Equal(time.Unix(100, 0)) {
		t.Errorf("entries = %+v", got)
	}
}
