package xo

import (
	"strings"
	"testing"
)

func TestParseConfig(t *testing.T) {
	cfg, err := ParseConfig("debian-12", map[string]string{
		"cpus": "2", "memory": "4G", "disk": "20GiB", "pool": "p", "sr": "Local storage", "network": "Pool-wide network",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Template: "debian-12", Pool: "p", SR: "Local storage", Network: "Pool-wide network", CPUs: 2, Memory: 4 << 30, Disk: 20 << 30}
	if cfg != want {
		t.Errorf("got %+v, want %+v", cfg, want)
	}

	cfg, err = ParseConfig("", map[string]string{"template": "Debian 12"})
	if err != nil || cfg.Template != "Debian 12" {
		t.Errorf("--template: %+v, %v", cfg, err)
	}

	cfg, err = ParseConfig("Other install media", map[string]string{"iso": "debian-13.iso", "disk": "20G"})
	if err != nil || cfg.ISO != "debian-13.iso" || cfg.Disk != 20<<30 {
		t.Errorf("--iso: %+v, %v", cfg, err)
	}
}

func TestParseConfigErrors(t *testing.T) {
	tests := []struct {
		source string
		flags  map[string]string
		err    string
	}{
		{"", nil, "missing template"},
		{"a", map[string]string{"template": "b"}, "not both"},
		{"a", map[string]string{"cpus": "0"}, "invalid --cpus"},
		{"a", map[string]string{"memory": "lots"}, "invalid --memory"},
		{"a", map[string]string{"disk": "-1G"}, "invalid --disk"},
		{"a", map[string]string{"ram": "1G"}, "unknown flag --ram"},
	}
	for _, tt := range tests {
		_, err := ParseConfig(tt.source, tt.flags)
		if err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("%q %v: got %v, want %q", tt.source, tt.flags, err, tt.err)
		}
	}
}

func TestParseStageConfig(t *testing.T) {
	cfg, err := ParseStageConfig(map[string]string{"cpus": "4", "disk": "30G", "network": "lan"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Config{CPUs: 4, Disk: 30 << 30, Network: "lan"}); cfg != want {
		t.Errorf("got %+v, want %+v", cfg, want)
	}
	for _, flag := range []string{"template", "pool", "iso"} {
		_, err := ParseStageConfig(map[string]string{flag: "x"})
		if err == nil || !strings.Contains(err.Error(), "--"+flag+" cannot be set when starting from another stage") {
			t.Errorf("--%s: got %v", flag, err)
		}
	}
	if _, err := ParseStageConfig(map[string]string{"ram": "1G"}); err == nil {
		t.Error("--ram: expected an error")
	}
}

func TestParseSize(t *testing.T) {
	tests := map[string]int64{
		"1024": 1024, "512M": 512 << 20, "4G": 4 << 30, "4g": 4 << 30, "20GiB": 20 << 30,
		"1T": 1 << 40, "8 GB": 8 << 30, "16K": 16 << 10,
	}
	for in, want := range tests {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "G", "1.5G", "0", "4X", "99999999999T"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q): expected an error", in)
		}
	}
}

func TestIPv4(t *testing.T) {
	tests := []struct {
		vm   vm
		want string
	}{
		{vm{MainIPAddress: "10.0.0.5"}, "10.0.0.5"},
		{vm{MainIPAddress: "fe80::1", Addresses: map[string]string{"0/ipv6/0": "fe80::1", "1/ipv4/0": "10.0.1.2", "0/ipv4/0": "10.0.0.2"}}, "10.0.0.2"},
		{vm{Addresses: map[string]string{"0/ipv6/0": "fe80::1"}}, ""},
		{vm{}, ""},
	}
	for _, tt := range tests {
		if got := ipv4(&tt.vm); got != tt.want {
			t.Errorf("%+v: got %q, want %q", tt.vm, got, tt.want)
		}
	}
}

func TestMatch(t *testing.T) {
	objs := []object{
		{ID: "t1", UUID: "t1", NameLabel: "Debian 12", Pool: "p1"},
		{ID: "t1", UUID: "t1", NameLabel: "Debian 12", Pool: "p2"},
		{ID: "t2", UUID: "t2", NameLabel: "Debian 13", Pool: "p1"},
		{ID: "p1-t3", UUID: "t3", NameLabel: "Other", Pool: "p1"},
		{ID: "t4", UUID: "t4", NameLabel: "Dup", Pool: "p1"},
		{ID: "t5", UUID: "t5", NameLabel: "Dup", Pool: "p1"},
		{ID: "t6", UUID: "t6", NameLabel: "t2", Pool: "p1"},
	}
	tests := []struct {
		ref, pool, wantID, wantPool, err string
	}{
		{"Debian 13", "", "t2", "p1", ""},
		{"t3", "", "p1-t3", "p1", ""},
		{"p1-t3", "", "p1-t3", "p1", ""},
		{"t1", "p2", "t1", "p2", ""},
		{"Debian 12", "p2", "t1", "p2", ""},
		{"t2", "", "t2", "p1", ""}, // an id wins over a name
		{"t1", "", "", "", "exists in 2 pools, set --pool"},
		{"Debian 12", "", "", "", "exists in 2 pools"},
		{"Dup", "", "", "", "2 templates match"},
		{"Debian 13", "p2", "", "", `no template "Debian 13" in pool p2`},
		{"nope", "", "", "", `no template "nope"`},
	}
	for _, tt := range tests {
		got, err := match(objs, "template", tt.ref, tt.pool)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%q in %q: got %v, want %q", tt.ref, tt.pool, err, tt.err)
			}
			continue
		}
		if err != nil || got.ID != tt.wantID || got.Pool != tt.wantPool {
			t.Errorf("%q in %q: got %+v, %v", tt.ref, tt.pool, got, err)
		}
	}
}
