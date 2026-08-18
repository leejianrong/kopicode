package build_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The sentinels. Chosen so a match cannot be an accident: no real describe
// output says v9.9.9, and the commit is not this repository's.
const (
	wantVersion   = "v9.9.9-806-gfeedface"
	wantCommit    = "feedfacecafebeef0123456789abcdef01234567"
	wantTreeState = "dirty"
	ldflagsPkg    = "github.com/leejianrong/kopicode/internal/build"
)

// TestLdflagsReachTheBinary is the end-to-end proof that the injection is real.
//
// Everything else about the build identity can be unit-tested from a pure
// function, and all of it would still be wrong if the Makefile's -X flags named
// a symbol the linker cannot find. `go build` does not fail on that. It sets
// nothing, and the binary ships with the zero value — so every session would
// report the same fabricated build, which is the exact defect KAN-806 exists to
// prevent, arriving through the mechanism meant to fix it.
//
// internal/arch/ldflags_test.go checks the Makefile's targets statically. This
// one compiles the real front ends and asks them, because a static check on the
// source and a working link are not the same claim.
func TestLdflagsReachTheBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles both front ends; runs under make test-all")
	}
	requireGoToolchain(t)

	ldflags := strings.Join([]string{
		"-X " + ldflagsPkg + ".version=" + wantVersion,
		"-X " + ldflagsPkg + ".commit=" + wantCommit,
		"-X " + ldflagsPkg + ".treeState=" + wantTreeState,
	}, " ")

	for _, cmdPkg := range []string{"kopicode", "kopibench"} {
		t.Run(cmdPkg, func(t *testing.T) {
			out := runVersion(t, cmdPkg, "-ldflags", ldflags)

			for _, want := range []string{wantVersion, wantCommit, wantTreeState, "ldflags"} {
				if !strings.Contains(out, want) {
					t.Errorf("--version = %q, missing %q\n"+
						"the -X flag did not reach the binary: check that %s declares that "+
						"variable and that the package is in the link graph",
						out, want, ldflagsPkg)
				}
			}
			if strings.Contains(out, "unknown") {
				t.Errorf("--version = %q, still reports an unknown field despite -X", out)
			}
		})
	}
}

// TestNoLdflagsAndNoVCSReportsUnknown is the other half, and the one that
// matters more.
//
// A user who runs `go build ./cmd/kopicode` or installs from the module cache
// never sees the Makefile. That binary must say it does not know what it is,
// because a fabricated build id pools with real ones and quietly widens an arm
// to cover two incomparable binaries — worse than a missing one, which at least
// refuses.
//
// -buildvcs=false reproduces the case without needing a checkout that has no
// git in it.
func TestNoLdflagsAndNoVCSReportsUnknown(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a front end; runs under make test-all")
	}
	requireGoToolchain(t)

	out := runVersion(t, "kopicode", "-buildvcs=false")

	for _, want := range []string{"unknown"} {
		if !strings.Contains(out, want) {
			t.Fatalf("--version = %q, want it to admit %q", out, want)
		}
	}
	for _, forbidden := range []string{"ldflags", wantCommit, "clean"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("--version = %q, but nothing recorded %q — a build with no identity "+
				"must not claim one", out, forbidden)
		}
	}
}

// runVersion builds cmd/<name> with the given extra build flags and returns
// what `--version` printed.
func runVersion(t *testing.T, name string, buildFlags ...string) string {
	t.Helper()

	root := moduleRoot(t)
	bin := filepath.Join(t.TempDir(), name)
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	args := append([]string{"build"}, buildFlags...)
	args = append(args, "-o", bin, "./cmd/"+name)

	build := exec.Command("go", args...)
	build.Dir = root
	build.Env = toolchainEnv()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	run := exec.Command(bin, "--version")
	run.Dir = t.TempDir()
	run.Env = processEnv()
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version: %v\n%s", bin, err, out)
	}
	return strings.TrimSpace(string(out))
}

// toolchainPassThrough is what `go build` needs to find itself and its caches.
// Everything else is dropped, and the drops are the whole point of the list.
//
// This test's entire subject is the artefact `go build` produced, so a variable
// that changes the artefact changes the answer while the command still reads
// correctly. GOFLAGS is the sharp one: it can carry -ldflags, which would
// override the very flags under test, and -tags, which changes what compiles.
// GOOS and GOARCH decide what the binary even is, and a developer with
// GOOS=windows exported would produce something the second half of runVersion
// then tries to execute.
//
// internal/corpus keeps an almost identical list for its oracles, and
// cmd/kopicode's frontends_integration_test.go keeps a third for the `go
// build` it runs to produce a binary to test (KAN-854). All three are
// duplicated rather than shared because internal/build must import nothing
// from this module — internal/arch's TestBuildPackageIsALeaf walks these test
// files too, and that leaf property is what lets both front ends import this
// package for --version at all — and because this variable is unexported in a
// _test.go file, so even a package allowed to import internal/build could not
// reach it. Adding a fourth builder for a `go build`/`go test` subprocess?
// Read all three before choosing a shape: KAN-854 is the record of a fourth
// one (cmd/kopicode's) having been a denylist by oversight rather than
// intent, and the fix was to make it match these two.
var toolchainPassThrough = []string{
	"PATH", "HOME", "TMPDIR", "TEMP", "TMP",
	"GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE", "GOPROXY",
	// GOTOOLCHAIN decides which compiler runs. It is passed through rather
	// than pinned because the `go test` process that got here resolved it the
	// same way, and forcing a different answer would test a build nobody makes.
	"GOTOOLCHAIN",
	// Windows needs these to start a process at all.
	"SystemRoot", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "ComSpec",
}

// processStartPassThrough is the smaller set: what an operating system needs to
// start a process, and nothing else.
var processStartPassThrough = []string{
	"PATH", "TEMP", "TMP",
	"SystemRoot", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "ComSpec",
}

// toolchainEnv is the environment the `go build` runs in.
//
// GOFLAGS is set empty rather than merely omitted. An allowlist keeps an
// inherited GOFLAGS out, but `go env -w GOFLAGS=…` writes it to a file the
// toolchain reads on its own, which no allowlist over the environment can
// reach; an explicit empty value in the environment overrides that file.
func toolchainEnv() []string {
	return append(envFrom(toolchainPassThrough), "GOFLAGS=")
}

// processEnv is what the freshly built binary runs under. It reports its
// identity from linker-injected variables and debug.ReadBuildInfo and reads no
// environment variable to do it, so anything beyond starting the process would
// be inherited for no reason.
func processEnv() []string {
	return envFrom(processStartPassThrough)
}

func envFrom(names []string) []string {
	env := make([]string, 0, len(names)+1)
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving module root: %v", err)
	}
	return root
}

func requireGoToolchain(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("no go toolchain on PATH: %v", err)
	}
}
