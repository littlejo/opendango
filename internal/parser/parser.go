// Package parser reads a Dangofile with the BuildKit Dockerfile parser and
// normalizes its AST into a flat list of commands.
//
// The BuildKit parser only knows the standard Dockerfile instructions. For any
// other keyword (CONNECT, REBOOT, OUTPUT, ...) it keeps the leading flags but
// drops the arguments, so those are recovered here from the original line.
package parser

import (
	"fmt"
	"io"
	"strings"

	bkparser "github.com/moby/buildkit/frontend/dockerfile/parser"
)

// Heredoc is a heredoc attached to a command (RUN <<EOF ...).
type Heredoc struct {
	Name    string
	Content string
}

// Command is one instruction of a Dangofile, before variable expansion.
type Command struct {
	// Keyword is the upper-cased instruction name (RUN, FROM, ...).
	Keyword string
	// Flags are the raw `--key=value` / `--key` flags.
	Flags []string
	// Args are the raw arguments. For standard instructions they come from
	// BuildKit; for custom ones they are the whitespace-separated words of the
	// original line, quotes included (expansion removes them later).
	Args []string
	// JSON is true for the exec form: RUN ["a", "b"].
	JSON     bool
	Heredocs []Heredoc
	// Original is the instruction text as written, continuations joined.
	Original  string
	StartLine int
	EndLine   int
}

// Result is a parsed Dangofile.
type Result struct {
	Commands []*Command
	// EscapeToken is the escape character set by the `# escape=` directive.
	EscapeToken rune
}

// standard lists the instructions the BuildKit parser understands; their
// arguments can be taken from the AST as is.
var standard = map[string]bool{
	"ADD": true, "ARG": true, "CMD": true, "COPY": true, "ENTRYPOINT": true,
	"ENV": true, "EXPOSE": true, "FROM": true, "HEALTHCHECK": true,
	"LABEL": true, "MAINTAINER": true, "ONBUILD": true, "RUN": true,
	"SHELL": true, "STOPSIGNAL": true, "USER": true, "VOLUME": true,
	"WORKDIR": true,
}

// Parse parses a Dangofile.
func Parse(r io.Reader) (*Result, error) {
	res, err := bkparser.Parse(r)
	if err != nil {
		return nil, err
	}
	out := &Result{EscapeToken: res.EscapeToken}
	for _, n := range res.AST.Children {
		cmd := &Command{
			Keyword:   strings.ToUpper(n.Value),
			Flags:     n.Flags,
			JSON:      n.Attributes["json"],
			Original:  n.Original,
			StartLine: n.StartLine,
			EndLine:   n.EndLine,
		}
		for _, h := range n.Heredocs {
			cmd.Heredocs = append(cmd.Heredocs, Heredoc{Name: h.Name, Content: h.Content})
		}
		if standard[cmd.Keyword] {
			for next := n.Next; next != nil; next = next.Next {
				cmd.Args = append(cmd.Args, next.Value)
			}
		} else {
			args, err := customArgs(n.Original)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", n.StartLine, err)
			}
			flags, err := customFlags(n.Original)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", n.StartLine, err)
			}
			cmd.Args, cmd.Flags = args, flags
		}
		out.Commands = append(out.Commands, cmd)
	}
	return out, nil
}

// customArgs splits the original line of a custom instruction into words,
// dropping the keyword and the flags. Unlike standard instructions, custom
// ones accept flags anywhere on the line (CONNECT ssh --user=debian); those
// are collected by customFlags.
func customArgs(original string) ([]string, error) {
	words, err := splitWords(original)
	if err != nil {
		return nil, err
	}
	var args []string
	for _, w := range words[1:] {
		if strings.HasPrefix(w, "--") {
			continue
		}
		args = append(args, w)
	}
	return args, nil
}

// customFlags returns every `--` word of a custom instruction, wherever it is
// on the line. BuildKit only extracts the flags placed before the first
// argument.
func customFlags(original string) ([]string, error) {
	words, err := splitWords(original)
	if err != nil {
		return nil, err
	}
	var flags []string
	for _, w := range words[1:] {
		if strings.HasPrefix(w, "--") {
			flags = append(flags, w)
		}
	}
	return flags, nil
}

// splitWords splits on unquoted whitespace and keeps the quotes, so that
// variable expansion can later process them like Docker does.
func splitWords(s string) ([]string, error) {
	var (
		words []string
		cur   strings.Builder
		quote rune
		in    bool
	)
	for _, r := range s {
		switch {
		case quote != 0:
			cur.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
			in = true
			cur.WriteRune(r)
		case r == ' ' || r == '\t' || r == '\n':
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			in = true
			cur.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	if in {
		words = append(words, cur.String())
	}
	return words, nil
}
