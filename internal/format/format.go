// Package format rewrites a Dangofile in its canonical layout. It only
// changes the layout around the instructions, never their meaning:
//
//   - keywords are upper-cased, with one space before the arguments;
//   - trailing spaces and tabs are removed;
//   - blank lines between instructions are reduced to one, and each FROM but
//     the first is preceded by one (above the comments attached to it);
//   - the file ends with a single newline.
//
// Heredoc bodies are left untouched, and so are the arguments: quotes,
// continuation lines and RUN scripts keep their layout.
package format

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"

	"github.com/littlejo/opendango/internal/parser"
)

// line kinds.
const (
	blank   = iota // empty line outside any instruction
	comment        // comment line outside any instruction
	first          // first line of an instruction
	inside         // other line of an instruction
	heredoc        // line of an instruction with a heredoc, after its first
)

// Format returns src in its canonical layout. src must be a valid Dangofile
// for the BuildKit parser.
func Format(src []byte) ([]byte, error) {
	res, err := parser.Parse(bytes.NewReader(src))
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(src), "\n")
	kinds := make([]int, len(lines))
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			kinds[i] = comment
		}
	}
	isFrom := map[int]bool{}
	for _, cmd := range res.Commands {
		start, end := cmd.StartLine-1, cmd.EndLine-1
		if start < 0 || end >= len(lines) || start > end {
			return nil, fmt.Errorf("line %d: unexpected instruction position", cmd.StartLine)
		}
		kinds[start] = first
		isFrom[start] = cmd.Keyword == "FROM"
		for i := start + 1; i <= end; i++ {
			if len(cmd.Heredocs) > 0 {
				kinds[i] = heredoc
			} else {
				kinds[i] = inside
			}
		}
	}

	var out []string
	seenInstruction := false
	for i, l := range lines {
		switch kinds[i] {
		case heredoc:
			out = append(out, l)
			continue
		case blank:
			// Kept only between two non-blank lines: collapsed, and dropped
			// at the start and the end of the file.
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
			continue
		case first:
			if isFrom[i] && seenInstruction {
				out = blankBeforeComments(out)
			}
			seenInstruction = true
			l = formatFirstLine(l)
		}
		out = append(out, strings.TrimRight(l, " \t"))
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return []byte(strings.Join(out, "\n") + "\n"), nil
}

// formatFirstLine upper-cases the keyword and leaves one space before the
// rest of the line.
func formatFirstLine(l string) string {
	l = strings.TrimLeft(l, " \t")
	end := strings.IndexFunc(l, unicode.IsSpace)
	if end < 0 {
		return strings.ToUpper(l)
	}
	rest := strings.TrimLeft(l[end:], " \t")
	if rest == "" {
		return strings.ToUpper(l[:end])
	}
	return strings.ToUpper(l[:end]) + " " + rest
}

// blankBeforeComments makes sure a blank line precedes the comments that
// end out, or the end of out when there are none.
func blankBeforeComments(out []string) []string {
	i := len(out)
	for i > 0 && strings.HasPrefix(strings.TrimLeft(out[i-1], " \t"), "#") {
		i--
	}
	if i == 0 || out[i-1] == "" {
		return out
	}
	return append(out[:i], append([]string{""}, out[i:]...)...)
}
