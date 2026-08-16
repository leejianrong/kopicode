package bench

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// goQueryTimeout bounds the one toolchain query a run makes. `go env` reads no
// network and touches no module, so a query that takes longer than this is a
// machine in trouble and the run is better off without an answer than waiting
// for one.
const goQueryTimeout = 15 * time.Second

// goCaches are the toolchain cache directories an oracle should use.
//
// The zero value is meaningful: an empty field means "say nothing about it",
// and the oracle then gets whatever the allowlist passed through or the
// toolchain derives from the temp HOME. That is the honest fallback — slower,
// never wrong.
type goCaches struct {
	// Build is GOCACHE.
	Build string
	// Mod is GOMODCACHE.
	Mod string
}

// resolveGoCaches asks the toolchain where its caches are, so a per-task temp
// HOME does not send every Go oracle to a cold cache and recompile the standard
// library once per task.
//
// It is best effort. A corpus with no Go task does not need `go` on PATH, and a
// failure here costs time rather than correctness — which is why it returns a
// value and no error, and why the caller logs rather than aborts.
//
// GOCACHE and GOMODCACHE already set in the ambient environment are left alone:
// the oracle allowlist passes those through, and a machine that has set them has
// said where it wants its caches.
func resolveGoCaches(ctx context.Context, dir string) (goCaches, error) {
	var out goCaches
	_, buildSet := os.LookupEnv("GOCACHE")
	_, modSet := os.LookupEnv("GOMODCACHE")
	if buildSet && modSet {
		return out, nil
	}

	ctx, cancel := context.WithTimeout(ctx, goQueryTimeout)
	defer cancel()

	// `go env` prints one value per line, in the order asked.
	lines, err := runGo(ctx, dir, "env", "GOCACHE", "GOMODCACHE")
	if err != nil {
		return out, err
	}
	if len(lines) > 0 && !buildSet {
		out.Build = lines[0]
	}
	if len(lines) > 1 && !modSet {
		out.Mod = lines[1]
	}
	return out, nil
}

// runGo runs one `go` command and returns its stdout as lines.
func runGo(ctx context.Context, dir string, args ...string) ([]string, error) {
	if dir == "" {
		return nil, fmt.Errorf("bench: go %s: no working directory: %w",
			strings.Join(args, " "), ErrNoWorkingDir)
	}

	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = goQueryEnv()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("bench: go %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return gitLines(stdout.String()), nil
}

// goQueryEnvNames are the variables `go env` needs in order to answer the
// question that is being asked of it.
//
// HOME is on the list, and it is the only reason this builder exists rather
// than [oracleEnv] being reused: `go env GOCACHE` *computes* its answer from
// HOME, and the answer wanted here is the one the user's toolchain would give,
// not the one a temp home would produce. GOFLAGS, GOOS, GOARCH and GOEXPERIMENT
// are deliberately absent — they change what a `go` command does, and this one
// is only being asked where a directory is.
var goQueryEnvNames = []string{
	"PATH", "HOME", "GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE",
	"SystemRoot", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "ComSpec",
}

// goQueryEnv is the environment the toolchain query runs under: the allowlist
// above and nothing else.
func goQueryEnv() []string {
	env := make([]string, 0, len(goQueryEnvNames))
	for _, name := range goQueryEnvNames {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}
