package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate", "-f", "../../examples/debian-nginx/Dangofile"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "is valid") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestValidateReportsLine(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"validate", "-f", "../../internal/plan/testdata/valid.dango", "--build-arg", "DEBIAN=13"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	code = run([]string{"validate", "-f", "testdata/invalid.dango"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "testdata/invalid.dango:3: EXPOSE has no meaning for a VM image") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 {
		t.Errorf("no args: exit %d", code)
	}
	if code := run([]string{"build", "--on-error=never"}, &stdout, &stderr); code != 2 {
		t.Errorf("--on-error=never: exit %d", code)
	}
	// go test runs without a terminal on stdin.
	stderr.Reset()
	if code := run([]string{"build", "--on-error=ask"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--on-error=ask needs a terminal") {
		t.Errorf("--on-error=ask: exit %d, stderr %q", code, stderr.String())
	}
	if code := run([]string{"nope"}, &stdout, &stderr); code != 2 {
		t.Errorf("unknown command: exit %d", code)
	}
}
