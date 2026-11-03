package format

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/littlejo/opendango/internal/parser"
)

func TestFormat(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{
			name: "keywords, spaces and final newline",
			src:  "  from   xo://t  \nconnect\tssh --user=debian\nrun    echo  'a  b'   ",
			want: "FROM xo://t\nCONNECT ssh --user=debian\nRUN echo  'a  b'\n",
		},
		{
			name: "blank lines",
			src:  "\n\nARG A=1\n\n\n\nFROM xo://t\nREBOOT\n\n\nOUTPUT --as=template\n\n\n",
			want: "ARG A=1\n\nFROM xo://t\nREBOOT\n\nOUTPUT --as=template\n",
		},
		{
			name: "blank line before FROM, above its comments",
			src:  "FROM xo://t AS a\nREBOOT\n# the second stage\n# starts from a\nFROM a AS b\nFROM b\n",
			want: "FROM xo://t AS a\nREBOOT\n\n# the second stage\n# starts from a\nFROM a AS b\n\nFROM b\n",
		},
		{
			name: "first FROM after directives and ARG",
			src:  "# escape=`\nARG T=x\nFROM xo://${T}\n",
			want: "# escape=`\nARG T=x\n\nFROM xo://${T}\n",
		},
		{
			name: "continuation lines keep their layout",
			src:  "FROM xo://t\nrun  apt-get update && \\   \n      apt-get install -y \\\n  nginx   \n",
			want: "FROM xo://t\nRUN apt-get update && \\\n      apt-get install -y \\\n  nginx\n",
		},
		{
			name: "heredoc bodies are untouched",
			src:  "FROM xo://t\nrun   <<EOF  \n  echo a   \n\n\n\tEOF2\nEOF\nCOPY a b\n",
			want: "FROM xo://t\nRUN <<EOF\n  echo a   \n\n\n\tEOF2\nEOF\nCOPY a b\n",
		},
		{
			name: "keyword alone",
			src:  "FROM xo://t\nreboot   \n",
			want: "FROM xo://t\nREBOOT\n",
		},
	}
	for _, tt := range tests {
		got, err := Format([]byte(tt.src))
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if string(got) != tt.want {
			t.Errorf("%s:\ngot  %q\nwant %q", tt.name, got, tt.want)
		}
		checkSame(t, tt.name, []byte(tt.src), got)
	}
}

func TestFormatInvalid(t *testing.T) {
	if _, err := Format([]byte("FROM xo://t\nRUN <<EOF\nnever closed")); err == nil {
		t.Error("expected a parse error")
	}
}

// TestFormatRepositoryFiles formats every Dangofile of the repository: the
// result must mean the same thing and be stable.
func TestFormatRepositoryFiles(t *testing.T) {
	var files []string
	for _, pattern := range []string{"../../examples/*/Dangofile", "../*/testdata/*.dango", "../../cmd/opendango/testdata/*.dango"} {
		m, _ := filepath.Glob(pattern)
		files = append(files, m...)
	}
	if len(files) < 3 {
		t.Fatalf("found only %v", files)
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Format(src)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		checkSame(t, f, src, got)
		again, _ := Format(got)
		if !bytes.Equal(again, got) {
			t.Errorf("%s: formatting is not stable:\n%s\n---\n%s", f, got, again)
		}
	}
}

// checkSame fails unless both sources parse to the same instructions.
func checkSame(t *testing.T, name string, a, b []byte) {
	t.Helper()
	pa, err := parser.Parse(bytes.NewReader(a))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	pb, err := parser.Parse(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("%s: formatted file does not parse: %v", name, err)
	}
	if len(pa.Commands) != len(pb.Commands) {
		t.Fatalf("%s: %d instructions, then %d", name, len(pa.Commands), len(pb.Commands))
	}
	for i := range pa.Commands {
		ca, cb := *pa.Commands[i], *pb.Commands[i]
		// Only the layout may change.
		ca.Original, cb.Original = "", ""
		ca.StartLine, ca.EndLine, cb.StartLine, cb.EndLine = 0, 0, 0, 0
		if !reflect.DeepEqual(ca, cb) {
			t.Errorf("%s: instruction %d changed:\n%+v\n%+v", name, i, ca, cb)
		}
	}
}
