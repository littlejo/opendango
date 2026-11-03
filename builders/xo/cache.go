package xo

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CacheEntry is an intermediate template of the cache.
type CacheEntry struct {
	ID      string
	Name    string
	Created time.Time
}

// ListCache returns the intermediate templates of the cache (tagged with
// the creation time), oldest first. The OUTPUT templates, only tagged with
// a cache key, are not listed.
func ListCache(ctx context.Context, c *Client) ([]CacheEntry, error) {
	objs, err := c.list(ctx, "vm-templates")
	if err != nil {
		return nil, err
	}
	return cacheEntries(objs), nil
}

func cacheEntries(objs []object) []CacheEntry {
	var entries []CacheEntry
	for _, o := range objs {
		for _, t := range o.Tags {
			unix, ok := strings.CutPrefix(t, createdTag)
			if !ok {
				continue
			}
			sec, err := strconv.ParseInt(unix, 10, 64)
			if err != nil {
				continue
			}
			entries = append(entries, CacheEntry{ID: o.ID, Name: o.NameLabel, Created: time.Unix(sec, 0)})
			break
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Created.Before(entries[j].Created) })
	return entries
}

// DeleteCache deletes an intermediate template of the cache.
func DeleteCache(ctx context.Context, c *Client, id string) error {
	return c.do(ctx, http.MethodDelete, "/vm-templates/"+url.PathEscape(id), nil, nil)
}
