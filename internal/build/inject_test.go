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
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	run := exec.Command(bin, "--version")
	run.Dir = t.TempDir()
	run.Env = os.Environ()
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version: %v\n%s", bin, err, out)
	}
	return strings.TrimSpace(string(out))
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
