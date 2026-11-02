package parser

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	src := `# a comment
ARG DEBIAN=12
FROM --cpus=2 xo://debian-${DEBIAN} AS golden
CONNECT ssh --user=debian
CONNECT --user=debian ssh
RUN apt-get update && \
    apt-get -y upgrade
RUN ["echo", "hi"]
RUN <<EOF
echo a
EOF
COPY --chmod=0600 a b /etc/
REBOOT
OUTPUT --as=template "golden debian"
OUTPUT --format=xva --compress=zstd ./dist/x.xva
`
	res, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	want := []Command{
		{Keyword: "ARG", Args: []string{"DEBIAN=12"}, StartLine: 2},
		{Keyword: "FROM", Flags: []string{"--cpus=2"}, Args: []string{"xo://debian-${DEBIAN}", "AS", "golden"}, StartLine: 3},
		{Keyword: "CONNECT", Flags: []string{"--user=debian"}, Args: []string{"ssh"}, StartLine: 4},
		{Keyword: "CONNECT", Flags: []string{"--user=debian"}, Args: []string{"ssh"}, StartLine: 5},
		{Keyword: "RUN", Args: []string{"apt-get update &&     apt-get -y upgrade"}, StartLine: 6},
		{Keyword: "RUN", Args: []string{"echo", "hi"}, JSON: true, StartLine: 8},
		{Keyword: "RUN", Args: []string{"<<EOF"}, Heredocs: []Heredoc{{Name: "EOF", Content: "echo a\n"}}, StartLine: 9},
		{Keyword: "COPY", Flags: []string{"--chmod=0600"}, Args: []string{"a", "b", "/etc/"}, StartLine: 12},
		{Keyword: "REBOOT", StartLine: 13},
		{Keyword: "OUTPUT", Flags: []string{"--as=template"}, Args: []string{`"golden debian"`}, StartLine: 14},
		{Keyword: "OUTPUT", Flags: []string{"--format=xva", "--compress=zstd"}, Args: []string{"./dist/x.xva"}, StartLine: 15},
	}
	if len(res.Commands) != len(want) {
		t.Fatalf("got %d commands, want %d", len(res.Commands), len(want))
	}
	for i, w := range want {
		got := *res.Commands[i]
		got.Original, got.EndLine = "", 0
		if len(got.Flags) == 0 {
			got.Flags = nil
		}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("command %d:\n got %#v\nwant %#v", i, got, w)
		}
	}
	if res.EscapeToken != '\\' {
		t.Errorf("escape token = %q", res.EscapeToken)
	}
}

func TestParseUnterminatedQuote(t *testing.T) {
	_, err := Parse(strings.NewReader("OUTPUT --as=template \"unterminated\n"))
	if err == nil || !strings.Contains(err.Error(), "line 1: unterminated quote") {
		t.Fatalf("got %v", err)
	}
}

func TestSplitWords(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{`OUTPUT a b`, []string{"OUTPUT", "a", "b"}},
		{`OUTPUT "a b" 'c d'`, []string{"OUTPUT", `"a b"`, `'c d'`}},
		{"X  a\t b", []string{"X", "a", "b"}},
		{`X --name="a b"`, []string{"X", `--name="a b"`}},
	}
	for _, tt := range tests {
		got, err := splitWords(tt.in)
		if err != nil {
			t.Fatalf("%q: %v", tt.in, err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q: got %q, want %q", tt.in, got, tt.want)
		}
	}
}
