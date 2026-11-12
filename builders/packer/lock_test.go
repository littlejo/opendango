package packer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPullLock(t *testing.T) {
	_, reg := newFakeRegistry(t)
	ctx := context.Background()
	lock := &Lock{File: filepath.Join(t.TempDir(), LockFile)}
	var out bytes.Buffer
	// opendangotest-local: the official plugin opendangotest; thing-x: the
	// GitHub plugin thing (no plugin is named thing-x).
	if err := reg.Pull(ctx, lock, []string{"opendangotest-local", "thing-x", "opendangotest-template"}, false, nil, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if err := lock.Write(); err != nil {
		t.Fatal(err)
	}
	read, err := ReadLock(lock.File)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Plugins) != 2 {
		t.Fatalf("plugins %+v", read.Plugins)
	}
	for name, want := range map[string]LockedPlugin{
		"opendangotest": {Source: "github.com/hashicorp/opendangotest", Version: "1.2.3"},
		"thing":         {Source: "github.com/acme/thing", Version: "2.0.0"},
	} {
		got := read.Plugins[name]
		if got == nil || got.Source != want.Source || got.Version != want.Version || len(got.Hashes[Platform()]) != 64 {
			t.Errorf("%s: %+v", name, got)
		}
	}

	// The lock finds the locked versions, with their hash.
	installed := Discover([]string{reg.Dir}, nil)
	if pl, builder, err := read.Find(installed, "thing-x"); err != nil || pl.Version != "2.0.0" || builder != "x" {
		t.Errorf("Find = %+v, %s, %v", pl, builder, err)
	}
	for _, tt := range []struct {
		change func(l *Lock)
		err    string
	}{
		{func(l *Lock) { delete(l.Plugins, "thing") }, "plugin: thing (for thing-x) is not in "},
		{func(l *Lock) { l.Plugins["thing"].Version = "9.9" }, "plugin: thing 9.9 ("},
		{func(l *Lock) { l.Plugins["thing"].Hashes[Platform()] = strings.Repeat("0", 64) }, "sha256 "},
		{func(l *Lock) { delete(l.Plugins["thing"].Hashes, Platform()) }, "has no hash of thing for " + Platform()},
	} {
		l, _ := ReadLock(lock.File)
		tt.change(l)
		if _, _, err := l.Find(installed, "thing-x"); err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("error = %v, want %q", err, tt.err)
		}
	}

	// A plugin the Dangofile does not use anymore leaves the lock; the
	// locked version stays.
	read.Plugins["opendangotest"].Hashes["plan9_mips"] = "abc"
	out.Reset()
	if err := reg.Pull(ctx, read, []string{"opendangotest-local"}, false, installed, &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := read.Plugins["thing"]; ok || !strings.Contains(out.String(), "Removing thing from ") {
		t.Errorf("plugins %v, output %q", read.Plugins, out.String())
	}
	if h := read.Plugins["opendangotest"].Hashes; h["plan9_mips"] != "abc" || len(h) != 2 {
		t.Errorf("hashes %v", h)
	}

	if err := reg.Pull(ctx, read, []string{"nothing-here"}, false, nil, &out); err == nil || !strings.Contains(err.Error(), `nothing-here: no plugin named "nothing-here" or a prefix of it`) {
		t.Errorf("error = %v", err)
	}
}

func TestReadLock(t *testing.T) {
	if l, err := ReadLock(filepath.Join(t.TempDir(), "none")); l != nil || err != nil {
		t.Errorf("missing file: %v, %v", l, err)
	}
	dir := t.TempDir()
	for content, want := range map[string]string{
		"plugins:\n  a:\n    source: github.com/o/a\n":                   "plugin a has no version",
		"plugins:\n  a:\n    source: nope\n    version: 1.0.0\n":         "invalid plugin source",
		"plugins:\n  a:\n    version: 1.0.0\n    extra: x\n":             "field extra not found",
		"plugins:\n  a:\n    source: github.com/o/a\n    version: 1.0\n": "",
	} {
		p := filepath.Join(dir, LockFile)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := ReadLock(p)
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%q: error %v, want %q", content, err, want)
		}
	}
}

func TestLockWriteQuotesVersions(t *testing.T) {
	l := &Lock{File: filepath.Join(t.TempDir(), LockFile), Plugins: map[string]*LockedPlugin{"a": {Source: "github.com/o/a", Version: "1.0", Hashes: map[string]string{}}}}
	if err := l.Write(); err != nil {
		t.Fatal(err)
	}
	read, err := ReadLock(l.File)
	if err != nil || read.Plugins["a"].Version != "1.0" {
		t.Errorf("%+v, %v", read, err)
	}
}
