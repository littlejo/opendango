package build

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// imagesCache keeps the artifacts made of files (a disk image) of the
// stages that other stages start from, by cache key, for the builders of
// the table whose artifact is files (packer.Known.OutputDir):
// <dir>/<key>/ holds the files, <dir>/<key>.files lists them. The zero
// value keeps nothing.
type imagesCache string

// files returns the files of the entry of key, false when there is none
// or a file is missing.
func (c imagesCache) files(key string) ([]string, bool) {
	if c == "" {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(string(c), key+".files"))
	if err != nil {
		return nil, false
	}
	var files []string
	for _, name := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		p := filepath.Join(string(c), key, filepath.FromSlash(name))
		if _, err := os.Stat(p); err != nil {
			return nil, false
		}
		files = append(files, p)
	}
	return files, len(files) > 0
}

// tmp returns a directory path that does not exist yet, next to the
// entries, for the plugin to create (its output directory must not
// exist).
func (c imagesCache) tmp(key string) (string, error) {
	if err := os.MkdirAll(string(c), 0o755); err != nil {
		return "", err
	}
	return newDirPath(string(c), key+".tmp-")
}

// commit makes the output directory tmp the entry of key, replacing the
// previous one, and returns where its files are. Files outside tmp are an
// error.
func (c imagesCache) commit(key, tmp string, files []string) ([]string, error) {
	var names []string
	for _, f := range files {
		rel, err := filepath.Rel(tmp, f)
		if err != nil || !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("%s is not in the output directory %s", f, tmp)
		}
		names = append(names, filepath.ToSlash(rel))
	}
	entry := filepath.Join(string(c), key)
	list := entry + ".files"
	if err := os.Remove(list); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.RemoveAll(entry); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, entry); err != nil {
		return nil, err
	}
	// The list last: an entry without it is not used.
	if err := os.WriteFile(list, []byte(strings.Join(names, "\n")+"\n"), 0o644); err != nil {
		return nil, err
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = filepath.Join(entry, filepath.FromSlash(n))
	}
	return out, nil
}

// ImageEntry is an entry of the images cache.
type ImageEntry struct {
	Key  string
	Dir  string
	Made time.Time
}

// ListImages lists the entries of the images cache in dir (empty: the
// default one), with the time they were made.
func ListImages(dir string) ([]ImageEntry, error) {
	if dir == "" {
		dir = defaultCacheDir("images")
	}
	if dir == "" {
		return nil, nil
	}
	lists, err := filepath.Glob(filepath.Join(dir, "*.files"))
	if err != nil {
		return nil, err
	}
	var entries []ImageEntry
	for _, l := range lists {
		info, err := os.Stat(l)
		if err != nil {
			continue
		}
		key := strings.TrimSuffix(filepath.Base(l), ".files")
		entries = append(entries, ImageEntry{Key: key, Dir: filepath.Join(dir, key), Made: info.ModTime()})
	}
	return entries, nil
}

// DeleteImage deletes an entry of the images cache.
func DeleteImage(e ImageEntry) error {
	if err := os.Remove(e.Dir + ".files"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.RemoveAll(e.Dir)
}

// defaultCacheDir is ~/.cache/opendango/<name>, "" when unknown.
func defaultCacheDir(name string) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "opendango", name)
}

// newDirPath returns a path under dir, starting with prefix, that does not
// exist.
func newDirPath(dir, prefix string) (string, error) {
	for range 10 {
		b := make([]byte, 6)
		rand.Read(b)
		p := filepath.Join(dir, prefix+hex.EncodeToString(b))
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			return p, nil
		}
	}
	return "", fmt.Errorf("no free directory name in %s", dir)
}

// largestFile returns the largest of files (the disk of an artifact made
// of files), "" when there is none.
func largestFile(files []string) string {
	best, size := "", int64(-1)
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Size() > size {
			best, size = f, info.Size()
		}
	}
	return best
}
