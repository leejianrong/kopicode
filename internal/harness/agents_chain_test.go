package harness_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/leejianrong/kopicode/internal/harness"
)

// TestLoadAgentsChainIsRootFirstAndStopsAtTheRepository is ADR-0032 decisions 1
// and 2: every AGENTS.md from the repository root down, nearest last, and
// nothing from above the root.
func TestLoadAgentsChainIsRootFirstAndStopsAtTheRepository(t *testing.T) {
	outer := t.TempDir()
	root := filepath.Join(outer, "repo")
	mid := filepath.Join(root, "services")
	deep := filepath.Join(mid, "api")
	for _, d := range []string{filepath.Join(root, ".git"), deep} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeAgentsFile(t, outer, "above")
	writeAgentsFile(t, root, "root")
	writeAgentsFile(t, deep, "deep") // no file in mid: one per directory that has one

	got, err := harness.LoadAgentsChain(deep)
	if err != nil {
		t.Fatalf("LoadAgentsChain: %v", err)
	}
	var contents []string
	for _, f := range got {
		contents = append(contents, f.Content)
	}
	if want := []string{"root", "deep"}; !slices.Equal(contents, want) {
		t.Errorf("chain = %v, want %v", contents, want)
	}
}

// TestLoadAgentsChainOfOneFileIsWhatLoadAgentsFileFinds: a repository with a
// single file behaves as before, from the root or from below it.
func TestLoadAgentsChainOfOneFileIsWhatLoadAgentsFileFinds(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAgentsFile(t, root, "only")

	one, ok, err := harness.LoadAgentsFile(deep)
	if err != nil || !ok {
		t.Fatalf("LoadAgentsFile: %v %v", ok, err)
	}
	chain, err := harness.LoadAgentsChain(deep)
	if err != nil || len(chain) != 1 || chain[0] != one {
		t.Errorf("chain = %+v (%v), want just %+v", chain, err, one)
	}
	none, err := harness.LoadAgentsChain(t.TempDir())
	if err != nil || len(none) != 0 {
		t.Errorf("a directory with no file gave %+v (%v)", none, err)
	}
}

// TestFitWithinCapDropsTheFurthestFirstAndKeepsTheNearest is ADR-0032 decision
// 4's arithmetic: whole files, furthest first, the last one never.
func TestFitWithinCapDropsTheFurthestFirstAndKeepsTheNearest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sizes []int
		cap   int
		want  []bool
	}{
		{"all fit", []int{10, 20, 30}, 60, []bool{true, true, true}},
		{"one over drops the furthest", []int{10, 20, 30}, 59, []bool{false, true, true}},
		{"two over", []int{10, 20, 30}, 40, []bool{false, false, true}},
		{"the nearest stays even alone over the cap", []int{10, 20, 300}, 100, []bool{false, false, true}},
		{"a lone file", []int{500}, 100, []bool{true}},
		{"none", nil, 100, []bool{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := harness.FitWithinCap(tc.sizes, tc.cap); !slices.Equal(got, tc.want) {
				t.Errorf("FitWithinCap(%v, %d) = %v, want %v", tc.sizes, tc.cap, got, tc.want)
			}
		})
	}
}
