// Package xo implements the Xen Orchestra builder (FROM xo://<template>).
//
// It talks to the XO REST API (/rest/v0), except for the conversion to a
// template, which the REST API does not expose yet and which goes through
// the JSON-RPC API (see jsonrpc.go).
package xo

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Config is the builder configuration, taken from the FROM line.
type Config struct {
	// Template is the name or UUID of the VM template to start from.
	Template string
	// Pool, SR and Network are names or UUIDs. Pool defaults to the
	// template's pool; SR and Network to the template's.
	Pool    string
	SR      string
	Network string
	// CPUs, Memory and Disk are zero when not set (template default).
	CPUs   int
	Memory int64 // bytes
	Disk   int64 // bytes, size of the first disk
}

// ParseConfig builds a Config from the FROM source and flags, without
// contacting Xen Orchestra.
func ParseConfig(source string, flags map[string]string) (Config, error) {
	cfg := Config{Template: source}
	for name, value := range flags {
		var err error
		switch name {
		case "template":
			if source != "" {
				return cfg, fmt.Errorf("xo: set the template either in the source or with --template, not both")
			}
			cfg.Template = value
		case "pool":
			cfg.Pool = value
		case "sr":
			cfg.SR = value
		case "network":
			cfg.Network = value
		case "cpus":
			cfg.CPUs, err = strconv.Atoi(value)
			if err != nil || cfg.CPUs < 1 {
				return cfg, fmt.Errorf("xo: invalid --cpus %q", value)
			}
		case "memory":
			if cfg.Memory, err = ParseSize(value); err != nil {
				return cfg, fmt.Errorf("xo: invalid --memory: %w", err)
			}
		case "disk":
			if cfg.Disk, err = ParseSize(value); err != nil {
				return cfg, fmt.Errorf("xo: invalid --disk: %w", err)
			}
		default:
			return cfg, fmt.Errorf("xo: unknown flag --%s (expected template, pool, sr, network, cpus, memory or disk)", name)
		}
	}
	if cfg.Template == "" {
		return cfg, fmt.Errorf("xo: missing template, use xo://<template> or --template=<template>")
	}
	return cfg, nil
}

var sizeRe = regexp.MustCompile(`^([0-9]+)\s*([KMGT]?)(IB|B)?$`)

// ParseSize parses a size such as 512M, 4G or 20GiB. Units are binary
// (1G = 1024^3 bytes); a bare number is in bytes.
func ParseSize(s string) (int64, error) {
	m := sizeRe.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil {
		return 0, fmt.Errorf("%q is not a size (examples: 512M, 4G, 20GiB)", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a positive size", s)
	}
	shift := map[string]uint{"": 0, "K": 10, "M": 20, "G": 30, "T": 40}[m[2]]
	if n > (1<<62)>>shift {
		return 0, fmt.Errorf("%q is too large", s)
	}
	return n << shift, nil
}
