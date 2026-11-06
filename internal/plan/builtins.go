package plan

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// The variables opendango computes for each build, like the predefined
// ARGs of Docker (TARGETPLATFORM): visible in the FROM lines and in
// dango.yaml, and in a stage that declares them (ARG DANGO_DATE). A
// --build-arg of the same name replaces one.
const (
	TimestampVar = "DANGO_TIMESTAMP"
	DateVar      = "DANGO_DATE"
	TimeVar      = "DANGO_TIME"
	BuildIDVar   = "DANGO_BUILD_ID"
	GitCommitVar = "DANGO_GIT_COMMIT"
)

// Volatile are the builtins that change at every build: the cache key
// counts them as fixed values, so that a name with a timestamp does not
// build everything again.
var Volatile = []string{TimestampVar, DateVar, TimeVar, BuildIDVar}

// Builtins computes the builtin variables of a build of the Dangofile in
// dir. Tests replace it.
var Builtins = func(dir string) map[string]string {
	now := time.Now().UTC()
	return map[string]string{
		TimestampVar: strconv.FormatInt(now.Unix(), 10),
		DateVar:      now.Format("20060102"),
		TimeVar:      now.Format("150405"),
		BuildIDVar:   newUUID(),
		GitCommitVar: gitCommit(dir),
	}
}

// newUUID returns a random (version 4) UUID.
func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// gitCommit returns the short commit of the git repository dir is in, ""
// outside of one or without git.
func gitCommit(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
