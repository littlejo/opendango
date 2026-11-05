package instructions

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moby/buildkit/frontend/dockerfile/shell"

	"github.com/littlejo/opendango/internal/parser"
)

// parse parses a one-instruction Dangofile.
func parse(t *testing.T, src string) *parser.Command {
	t.Helper()
	res, err := parser.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("%q: %v", src, err)
	}
	if len(res.Commands) != 1 {
		t.Fatalf("%q: got %d commands", src, len(res.Commands))
	}
	return res.Commands[0]
}

// expander expands with V=val and removes quotes, like the plan does.
func expander() Expander {
	lex := shell.NewLex('\\')
	env := shell.EnvsFromSlice([]string{"V=val"})
	return func(w string) (string, error) {
		res, err := lex.ProcessWordWithMatches(w, env)
		return res.Result, err
	}
}

// checkErr fails unless err contains want (want == "" means no error).
func checkErr(t *testing.T, src string, err error, want string) bool {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Errorf("%q: unexpected error: %v", src, err)
	case want != "" && err == nil:
		t.Errorf("%q: expected error %q", src, want)
	case want != "" && !strings.Contains(err.Error(), want):
		t.Errorf("%q: error %q does not contain %q", src, err, want)
	default:
		return want == ""
	}
	return false
}

func TestParseArg(t *testing.T) {
	tests := []struct {
		src  string
		want []*Arg
		err  string
	}{
		{"ARG A", []*Arg{{Pos: Pos{1}, Name: "A"}}, ""},
		{"ARG A=1 B=${V}", []*Arg{{Pos: Pos{1}, Name: "A", Default: "1", HasDefault: true}, {Pos: Pos{1}, Name: "B", Default: "val", HasDefault: true}}, ""},
		{`ARG A="x y"`, []*Arg{{Pos: Pos{1}, Name: "A", Default: "x y", HasDefault: true}}, ""},
		{"ARG A=", []*Arg{{Pos: Pos{1}, Name: "A", HasDefault: true}}, ""},
		{"ARG 1A", nil, "invalid variable name"},
		{"ARG --x A", nil, "unknown flag"},
	}
	for _, tt := range tests {
		got, err := ParseArg(parse(t, tt.src), expander())
		if checkErr(t, tt.src, err, tt.err) && !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q: got %+v, want %+v", tt.src, got, tt.want)
		}
	}
}

func TestParseFrom(t *testing.T) {
	tests := []struct {
		src  string
		want *From
		err  string
	}{
		{"FROM xo://debian-12", &From{Pos: Pos{1}, Builder: "xo", Source: "debian-12", Flags: map[string]string{}}, ""},
		{"FROM xo://debian-${V} AS golden", &From{Pos: Pos{1}, Builder: "xo", Source: "debian-val", Name: "golden", Flags: map[string]string{}}, ""},
		{"FROM --cpus=2 --sr=${V} xo://t as g", &From{Pos: Pos{1}, Builder: "xo", Source: "t", Name: "g", Flags: map[string]string{"cpus": "2", "sr": "val"}}, ""},
		{`FROM --template="Debian 12" xo://`, &From{Pos: Pos{1}, Builder: "xo", Flags: map[string]string{"template": "Debian 12"}}, ""},
		{"FROM golden", &From{Pos: Pos{1}, Stage: "golden", Flags: map[string]string{}}, ""},
		{"FROM --cpus=4 golden AS app", &From{Pos: Pos{1}, Stage: "golden", Name: "app", Flags: map[string]string{"cpus": "4"}}, ""},
		{"FROM 1golden", nil, "or the name of an earlier stage"},
		{"FROM ://x", nil, "missing builder name"},
		{"FROM xo://a b", nil, "exactly one source"},
		{"FROM xo://a AS 1x", nil, "invalid stage name"},
		{"FROM --cpus xo://a", nil, "requires a value"},
		{"FROM --cpus=1 --cpus=2 xo://a", nil, "set twice"},
	}
	for _, tt := range tests {
		got, err := ParseFrom(parse(t, tt.src), expander())
		if checkErr(t, tt.src, err, tt.err) && !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q: got %+v, want %+v", tt.src, got, tt.want)
		}
	}
}

func TestParseConnect(t *testing.T) {
	tests := []struct {
		src  string
		want *Connect
		err  string
	}{
		{"CONNECT ssh --user=debian", &Connect{Pos: Pos{1}, Protocol: "ssh", User: "debian", Port: 22, Timeout: 10 * time.Minute}, ""},
		{"CONNECT --user=${V} --port=2222 --timeout=30s ssh", &Connect{Pos: Pos{1}, Protocol: "ssh", User: "val", Port: 2222, Timeout: 30 * time.Second}, ""},
		{"CONNECT ssh", nil, "requires --user"},
		{"CONNECT --user=a", nil, "exactly one protocol"},
		{"CONNECT winrm --user=a", nil, "not supported yet"},
		{"CONNECT telnet --user=a", nil, "unknown protocol"},
		{"CONNECT ssh --user=a --port=0", nil, "invalid --port"},
		{"CONNECT ssh --user=a --timeout=soon", nil, "invalid --timeout"},
		{"CONNECT ssh --user=a --password=x", nil, "unknown flag --password"},
	}
	for _, tt := range tests {
		got, err := ParseConnect(parse(t, tt.src), expander())
		if checkErr(t, tt.src, err, tt.err) && !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q: got %+v, want %+v", tt.src, got, tt.want)
		}
	}
}

func TestParseEnv(t *testing.T) {
	tests := []struct {
		src  string
		want *Env
		err  string
	}{
		{"ENV A=1", &Env{Pos: Pos{1}, Vars: []KeyValue{{"A", "1"}}}, ""},
		{`ENV A=1 B="x y" C=${V}`, &Env{Pos: Pos{1}, Vars: []KeyValue{{"A", "1"}, {"B", "x y"}, {"C", "val"}}}, ""},
		{"ENV A some value", &Env{Pos: Pos{1}, Vars: []KeyValue{{"A", "some value"}}}, ""},
		{"ENV --persist A=1", &Env{Pos: Pos{1}, Vars: []KeyValue{{"A", "1"}}, Persist: true}, ""},
		{`ENV --persist A="a\"b"`, nil, "cannot contain a double quote"},
		{"ENV --persist=yes A=1", nil, "takes no value"},
		{"ENV 1A=1", nil, "invalid variable name"},
	}
	for _, tt := range tests {
		got, err := ParseEnv(parse(t, tt.src), expander())
		if checkErr(t, tt.src, err, tt.err) && !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q: got %+v, want %+v", tt.src, got, tt.want)
		}
	}
}

func TestParseUser(t *testing.T) {
	tests := []struct {
		src  string
		want *User
		err  string
	}{
		{"USER root", &User{Pos: Pos{1}, Name: "root"}, ""},
		{"USER ${V}", &User{Pos: Pos{1}, Name: "val"}, ""},
		{"USER a:b", nil, "groups are not supported"},
		{"USER a b", nil, "invalid user name"},
		{"USER ${UNSET}", nil, "empty argument"},
	}
	for _, tt := range tests {
		got, err := ParseUser(parse(t, tt.src), expander())
		if checkErr(t, tt.src, err, tt.err) && !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q: got %+v, want %+v", tt.src, got, tt.want)
		}
	}
}

func TestParseWorkdir(t *testing.T) {
	tests := []struct {
		src  string
		want *Workdir
		err  string
	}{
		{"WORKDIR /opt/${V}", &Workdir{Pos: Pos{1}, Path: "/opt/val"}, ""},
		{"WORKDIR rel", &Workdir{Pos: Pos{1}, Path: "rel"}, ""},
		{"WORKDIR /a b", &Workdir{Pos: Pos{1}, Path: "/a b"}, ""},
		{"WORKDIR --x /a", nil, "unknown flag"},
	}
	for _, tt := range tests {
		got, err := ParseWorkdir(parse(t, tt.src), expander())
		if checkErr(t, tt.src, err, tt.err) && !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q: got %+v, want %+v", tt.src, got, tt.want)
		}
	}
}

func TestParseRun(t *testing.T) {
	tests := []struct {
		src  string
		want string
		exec bool
		err  string
	}{
		{"RUN echo $HOME", "echo $HOME", false, ""},
		{`RUN ["echo", "it's"]`, `exec 'echo' 'it'\''s'`, true, ""},
		{"RUN <<EOF\necho a\necho b\nEOF", "echo a\necho b\n", false, ""},
		{"RUN cat > /f <<EOF\nx\nEOF", "cat > /f <<EOF\nx\nEOF\n", false, ""},
		{"RUN --mount=type=cache echo", "", false, "unknown flag"},
	}
	for _, tt := range tests {
		got, err := ParseRun(parse(t, tt.src))
		if checkErr(t, tt.src, err, tt.err) && (got.Script != tt.want || got.Exec != tt.exec) {
			t.Errorf("%q: got script %q (exec %v), want %q (exec %v)", tt.src, got.Script, got.Exec, tt.want, tt.exec)
		}
	}
}

func TestParseLabel(t *testing.T) {
	tests := []struct {
		src  string
		want []KeyValue
		err  string
	}{
		{`LABEL a=b "c d"="e f" org.x=${V}`, []KeyValue{{"a", "b"}, {"c d", "e f"}, {"org.x", "val"}}, ""},
		{"LABEL k v w", []KeyValue{{"k", "v w"}}, ""},
		{`LABEL ""=x`, nil, "empty key"},
		{"LABEL opendango-cache-created:1=x", nil, "reserved for the cache"},
		{"LABEL --x=1 a=b", nil, "unknown flag"},
	}
	for _, tt := range tests {
		got, err := ParseLabel(parse(t, tt.src), expander())
		if checkErr(t, tt.src, err, tt.err) && !reflect.DeepEqual(got, &Label{Pos: Pos{1}, Labels: tt.want}) {
			t.Errorf("%q: got %+v, want %+v", tt.src, got, tt.want)
		}
	}
}

func TestParseAdd(t *testing.T) {
	sum := strings.Repeat("0", 64)
	tests := []struct {
		src  string
		want *Add
		err  string
	}{
		{"ADD --chown=www a https://x.org/${V}.tgz /srv/", &Add{Copy: Copy{Pos: Pos{1}, Sources: []string{"a", "https://x.org/val.tgz"}, Dest: "/srv/", Chown: "www"}}, ""},
		{"ADD --checksum=sha256:" + sum + " http://x.org/f /f", &Add{Copy: Copy{Pos: Pos{1}, Sources: []string{"http://x.org/f"}, Dest: "/f"}, Checksum: "sha256:" + sum}, ""},
		{"ADD --checksum=sha256:" + sum + " a /f", nil, "--checksum requires exactly one http(s) URL source"},
		{"ADD --checksum=sha256:" + sum + " http://x/a http://x/b /d/", nil, "--checksum requires exactly one"},
		{"ADD --checksum=md5:abc http://x/f /f", nil, "invalid --checksum"},
		{"ADD --checksum=sha256:ABC http://x/f /f", nil, "invalid --checksum"},
		{"ADD https://github.com/x/y.git /src", nil, "git sources are not supported yet"},
		{"ADD git@github.com:x/y.git /src", nil, "git sources are not supported yet"},
		{"ADD --keep-git-dir a b", nil, "unknown flag --keep-git-dir"},
		{"ADD --chmod=rw a b", nil, "ADD: invalid --chmod"},
		{"ADD a", nil, "ADD requires at least one source and a destination"},
		{`ADD ["a", "b"]`, nil, "ADD: the JSON form"},
	}
	for _, tt := range tests {
		got, err := ParseAdd(parse(t, tt.src), expander())
		if checkErr(t, tt.src, err, tt.err) && !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q: got %+v, want %+v", tt.src, got, tt.want)
		}
	}
}

func TestParseShell(t *testing.T) {
	tests := []struct {
		src  string
		want []string
		err  string
	}{
		{`SHELL ["/bin/bash", "-o", "pipefail", "-c"]`, []string{"/bin/bash", "-o", "pipefail", "-c"}, ""},
		// Not expanded, like in Docker.
		{`SHELL ["${V}"]`, []string{"${V}"}, ""},
		{"SHELL /bin/bash -c", nil, "requires the JSON form"},
		{"SHELL []", nil, "requires at least the executable"},
		{`SHELL [""]`, nil, "requires at least the executable"},
		{`SHELL --x=1 ["sh"]`, nil, "unknown flag"},
	}
	for _, tt := range tests {
		got, err := ParseShell(parse(t, tt.src))
		if checkErr(t, tt.src, err, tt.err) && !reflect.DeepEqual(got, &Shell{Pos: Pos{1}, Args: tt.want}) {
			t.Errorf("%q: got %+v, want %q", tt.src, got, tt.want)
		}
	}
}

func TestParseCopy(t *testing.T) {
	tests := []struct {
		src  string
		want *Copy
		err  string
	}{
		{"COPY a /b", &Copy{Pos: Pos{1}, Sources: []string{"a"}, Dest: "/b"}, ""},
		{"COPY --chmod=0600 --chown=www:www a ${V}/ /etc/", &Copy{Pos: Pos{1}, Sources: []string{"a", "val/"}, Dest: "/etc/", Chmod: "0600", Chown: "www:www"}, ""},
		{"COPY a", nil, "at least one source"},
		{"COPY --chmod=rw a b", nil, "invalid --chmod"},
		{"COPY --chown= a b", nil, "invalid --chown"},
		// Paths from the root of the source stage's machine.
		{"COPY --from=${V} --chmod=0755 /opt/app etc/../etc/app.conf /usr/local/", &Copy{Pos: Pos{1}, Sources: []string{"/opt/app", "/etc/app.conf"}, Dest: "/usr/local/", Chmod: "0755", From: "val"}, ""},
		{"COPY --from= a b", nil, "--from needs the name of a stage"},
		{"COPY --from=x /opt/*.so b", nil, "patterns are not supported"},
		{"COPY --from=x / b", nil, "the whole machine (/) cannot be copied"},
		{`COPY ["a", "b"]`, nil, "JSON form"},
	}
	for _, tt := range tests {
		got, err := ParseCopy(parse(t, tt.src), expander())
		if checkErr(t, tt.src, err, tt.err) && !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q: got %+v, want %+v", tt.src, got, tt.want)
		}
	}
}

func TestParseReboot(t *testing.T) {
	tests := []struct {
		src string
		err string
	}{
		{"REBOOT", ""},
		{"REBOOT now", "takes no argument"},
		{"REBOOT --timeout=1m", "unknown flag"},
	}
	for _, tt := range tests {
		got, err := ParseReboot(parse(t, tt.src))
		if checkErr(t, tt.src, err, tt.err) && got.Line() != 1 {
			t.Errorf("%q: line %d", tt.src, got.Line())
		}
	}
}

func TestParseBreakpoint(t *testing.T) {
	tests := []struct {
		src  string
		want string
		err  string
	}{
		{"BREAKPOINT", "", ""},
		{`BREAKPOINT "check nginx"`, "check nginx", ""},
		{"BREAKPOINT check   ${V}", "check val", ""},
		{"BREAKPOINT --now", "", "unknown flag"},
	}
	for _, tt := range tests {
		got, err := ParseBreakpoint(parse(t, tt.src), expander())
		if checkErr(t, tt.src, err, tt.err) && (got.Message != tt.want || got.Line() != 1) {
			t.Errorf("%q: got %+v, want message %q", tt.src, got, tt.want)
		}
	}
}

func TestParseServe(t *testing.T) {
	tests := []struct {
		src  string
		want Serve
		err  string
	}{
		{"SERVE http", Serve{Dir: "http"}, ""},
		{"SERVE --port=8080 --addr=10.0.0.5 http/${V}", Serve{Dir: "http/val", Port: 8080, Addr: "10.0.0.5"}, ""},
		{"SERVE http --port=8080", Serve{Dir: "http", Port: 8080}, ""},
		{"SERVE .", Serve{Dir: "."}, ""},
		{"SERVE", Serve{}, "exactly one directory"},
		{"SERVE a b", Serve{}, "exactly one directory"},
		{"SERVE ../http", Serve{}, "outside the Dangofile directory"},
		{"SERVE /srv/http", Serve{}, "outside the Dangofile directory"},
		{"SERVE --port=0 http", Serve{}, "invalid --port"},
		{"SERVE --port=web http", Serve{}, "invalid --port"},
		{"SERVE --addr=host.lan http", Serve{}, "invalid --addr"},
		{"SERVE --bind=0.0.0.0 http", Serve{}, "unknown flag"},
	}
	for _, tt := range tests {
		got, err := ParseServe(parse(t, tt.src), expander())
		if !checkErr(t, tt.src, err, tt.err) {
			continue
		}
		tt.want.Pos = Pos{StartLine: 1}
		if *got != tt.want {
			t.Errorf("%q: got %+v, want %+v", tt.src, *got, tt.want)
		}
	}
}

func TestParseOutput(t *testing.T) {
	tests := []struct {
		src  string
		want *Output
		err  string
	}{
		{"OUTPUT --as=template", &Output{Pos: Pos{1}, Kind: OutputTemplate}, ""},
		{`OUTPUT --as=template "golden ${V}"`, &Output{Pos: Pos{1}, Kind: OutputTemplate, Name: "golden val"}, ""},
		{"OUTPUT --format=xva ./dist/${V}.xva", &Output{Pos: Pos{1}, Kind: OutputXVA, Path: "./dist/val.xva"}, ""},
		{"OUTPUT ./x.xva --format=xva --compress=zstd", &Output{Pos: Pos{1}, Kind: OutputXVA, Path: "./x.xva", Compress: "zstd"}, ""},
		{"OUTPUT x", nil, "exactly one of"},
		{"OUTPUT --as=template --format=xva x", nil, "exactly one of"},
		{"OUTPUT --as=image", nil, "unknown --as"},
		{"OUTPUT --as=template --compress=zstd", nil, "--compress is for --format=xva"},
		{"OUTPUT --as=template a b", nil, "at most one name"},
		{"OUTPUT --format=qcow2 x", nil, "unknown --format"},
		{"OUTPUT --format=xva", nil, "exactly one destination"},
		{"OUTPUT --format=xva --compress=xz x", nil, "unknown --compress"},
		// The post-processors.
		{"OUTPUT --format=manifest dist/manifest.json", &Output{Pos: Pos{1}, Kind: OutputManifest, Path: "dist/manifest.json"}, ""},
		{"OUTPUT --format=checksum dist/SHA256SUMS", &Output{Pos: Pos{1}, Kind: OutputChecksum, Path: "dist/SHA256SUMS", Type: "sha256"}, ""},
		{"OUTPUT --format=checksum --type=sha512 dist/SUMS", &Output{Pos: Pos{1}, Kind: OutputChecksum, Path: "dist/SUMS", Type: "sha512"}, ""},
		{"OUTPUT --format=compress dist/${V}.tar.gz", &Output{Pos: Pos{1}, Kind: OutputArchive, Path: "dist/val.tar.gz"}, ""},
		{"OUTPUT --output=dist/${V}.box --compression_level=6 plugin://vagrant", &Output{Pos: Pos{1}, Kind: OutputPlugin, Source: "vagrant", Settings: map[string]string{"output": "dist/val.box", "compression_level": "6"}}, ""},
		{"OUTPUT --format=checksum --type=crc32 a", nil, "unknown --type"},
		{"OUTPUT --format=manifest --type=md5 a", nil, "--type is for --format=checksum"},
		{"OUTPUT --format=compress a.rar", nil, "the extension gives the format"},
		{"OUTPUT --format=manifest", nil, "requires exactly one destination path"},
		{"OUTPUT --keep plugin://vagrant", nil, "flag --keep requires a value"},
		{"OUTPUT plugin://", nil, "missing post-processor"},
	}
	for _, tt := range tests {
		got, err := ParseOutput(parse(t, tt.src), expander())
		if checkErr(t, tt.src, err, tt.err) && !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q: got %+v, want %+v", tt.src, got, tt.want)
		}
	}
}

func TestParseProvision(t *testing.T) {
	tests := []struct {
		src  string
		want *Provision
		err  string
	}{
		{`PROVISION --playbook_file=${V}.yml --extra_arguments='["--tags", "web"]' plugin://ansible`, &Provision{Pos: Pos{1}, Source: "ansible", Settings: map[string]string{"playbook_file": "val.yml", "extra_arguments": `["--tags", "web"]`}}, ""},
		{"PROVISION plugin://ansible-local", &Provision{Pos: Pos{1}, Source: "ansible-local", Settings: map[string]string{}}, ""},
		{"PROVISION ansible", nil, "requires one plugin://<provisioner>"},
		{"PROVISION plugin://", nil, "requires one plugin://<provisioner>"},
		{"PROVISION plugin://a plugin://b", nil, "requires one plugin://<provisioner>"},
		{"PROVISION --become plugin://ansible", nil, "flag --become requires a value"},
	}
	for _, tt := range tests {
		got, err := ParseProvision(parse(t, tt.src), expander())
		if checkErr(t, tt.src, err, tt.err) && !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q: got %+v, want %+v", tt.src, got, tt.want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	if got := ShellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("got %s", got)
	}
}

func TestParseBoot(t *testing.T) {
	tests := []struct {
		src  string
		want string
		err  string
	}{
		{"BOOT <esc><wait>auto url=http://${V}/ks.cfg<enter>", "<esc><wait>auto url=http://val/ks.cfg<enter>", ""},
		{"BOOT <leftCtrlOn>c<leftCtrlOff> --- quiet", "<leftCtrlOn>c<leftCtrlOff> --- quiet", ""},
		{"BOOT", "", "requires the keys to type"},
		{"BOOT <wait0s>", "", "Expecting a positive wait value"},
	}
	for _, tt := range tests {
		got, err := ParseBoot(parse(t, tt.src), expander())
		if checkErr(t, tt.src, err, tt.err) && (got.Keys != tt.want || got.Line() != 1) {
			t.Errorf("%q: got %+v, want keys %q", tt.src, got, tt.want)
		}
	}
}
