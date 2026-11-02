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
