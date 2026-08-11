package build

import (
	"runtime/debug"
	"strings"
	"testing"
)

// none is the reader for a binary that carries no build info at all.
func none() (*debug.BuildInfo, bool) { return nil, false }

func withSettings(mainVersion string, kv ...string) func() (*debug.BuildInfo, bool) {
	bi := &debug.BuildInfo{}
	bi.Main.Version = mainVersion
	for i := 0; i+1 < len(kv); i += 2 {
		bi.Settings = append(bi.Settings, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
	}
	return func() (*debug.BuildInfo, bool) { return bi, true }
}

// TestResolveNeverFabricates is the point of the package. Every path that has
// no evidence must say "unknown", because a plausible-looking default is worse
// than a missing one: it pools.
func TestResolveNeverFabricates(t *testing.T) {
	cases := []struct {
		name                    string
		version, commit, treeSt string
		read                    func() (*debug.BuildInfo, bool)
		want                    Info
	}{
		{
			name: "no ldflags, no build info at all",
			read: none,
			want: Info{Version: Unknown, Commit: Unknown, TreeState: TreeUnknown, Source: SourceUnknown},
		},
		{
			name: "no ldflags, build info with nothing usable",
			read: withSettings("", "GOOS", "linux"),
			want: Info{Version: Unknown, Commit: Unknown, TreeState: TreeUnknown, Source: SourceUnknown},
		},
		{
			name: "no ldflags, (devel) module version and no vcs stamps",
			read: withSettings("(devel)"),
			want: Info{Version: Unknown, Commit: Unknown, TreeState: TreeUnknown, Source: SourceUnknown},
		},
		{
			name: "go install from the module cache",
			read: withSettings("v0.4.1"),
			want: Info{Version: "v0.4.1", Commit: Unknown, TreeState: TreeUnknown, Source: SourceBuildInfo},
		},
		{
			name: "go build from a clean checkout, no ldflags",
			read: withSettings("(devel)", "vcs.revision", "abc123", "vcs.modified", "false"),
			want: Info{Version: Unknown, Commit: "abc123", TreeState: TreeClean, Source: SourceBuildInfo},
		},
		{
			name: "go build from a dirty checkout, no ldflags",
			read: withSettings("(devel)", "vcs.revision", "abc123", "vcs.modified", "true"),
			want: Info{Version: Unknown, Commit: "abc123", TreeState: TreeDirty, Source: SourceBuildInfo},
		},
		{
			name:    "ldflags win over build info, and are not mixed with it",
			version: "v1.2.0", commit: "deadbeef", treeSt: "clean",
			read: withSettings("v0.4.1", "vcs.revision", "999999", "vcs.modified", "true"),
			want: Info{Version: "v1.2.0", Commit: "deadbeef", TreeState: TreeClean, Source: SourceLDFlags},
		},
		{
			name:   "ldflags with a commit but no version",
			commit: "deadbeef", treeSt: "dirty",
			read: none,
			want: Info{Version: Unknown, Commit: "deadbeef", TreeState: TreeDirty, Source: SourceLDFlags},
		},
		{
			name:    "a version with no commit is not an identity",
			version: "v1.2.0", treeSt: "clean",
			read: none,
			want: Info{Version: Unknown, Commit: Unknown, TreeState: TreeUnknown, Source: SourceUnknown},
		},
		{
			name:    "an unrecognised tree state degrades to unknown, not to clean",
			version: "v1.2.0", commit: "deadbeef", treeSt: "probably fine",
			read: none,
			want: Info{Version: "v1.2.0", Commit: "deadbeef", TreeState: TreeUnknown, Source: SourceLDFlags},
		},
		{
			name:    "an empty tree state degrades to unknown",
			version: "v1.2.0", commit: "deadbeef",
			read: none,
			want: Info{Version: "v1.2.0", Commit: "deadbeef", TreeState: TreeUnknown, Source: SourceLDFlags},
		},
		{
			name:    "whitespace-only ldflags are no ldflags",
			version: "  ", commit: "  ", treeSt: " ",
			read: none,
			want: Info{Version: Unknown, Commit: Unknown, TreeState: TreeUnknown, Source: SourceUnknown},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolve(tc.version, tc.commit, tc.treeSt, tc.read)
			if got != tc.want {
				t.Errorf("resolve() = %+v\n           want %+v", got, tc.want)
			}
		})
	}
}

// TestEveryFieldIsAlwaysPopulated: a zero-valued field in a journal reads as
// "not implemented yet" rather than as "not known", and the two call for
// different actions. There is no empty string in a resolved Info.
func TestEveryFieldIsAlwaysPopulated(t *testing.T) {
	reads := map[string]func() (*debug.BuildInfo, bool){
		"none":     none,
		"empty":    withSettings(""),
		"devel":    withSettings("(devel)"),
		"released": withSettings("v0.4.1"),
		"vcs":      withSettings("(devel)", "vcs.revision", "abc", "vcs.modified", "true"),
	}
	for name, read := range reads {
		for _, ld := range []struct{ version, commit, treeSt string }{
			{},
			{"v1", "abc", "clean"},
			{"", "abc", ""},
		} {
			got := resolve(ld.version, ld.commit, ld.treeSt, read)
			if got.Version == "" || got.Commit == "" || got.TreeState == "" || got.Source == "" {
				t.Errorf("%s + %+v: resolve left a field empty: %+v", name, ld, got)
			}
		}
	}
}

// TestCurrentResolvesUnderTest guards the live path rather than the pure
// function. A test binary carries no VCS stamps and no ldflags, so the honest
// answer here is "unknown" — and if it ever starts claiming something else,
// that is the fabrication this package exists to prevent, arriving through the
// one path the table above cannot see.
func TestCurrentResolvesUnderTest(t *testing.T) {
	got := Current()
	if got.Version == "" || got.Commit == "" || got.TreeState == "" || got.Source == "" {
		t.Fatalf("Current() left a field empty: %+v", got)
	}
	if got.Source == SourceLDFlags {
		t.Errorf("Current() = %+v, but a test binary is not built with the Makefile's ldflags", got)
	}
}

// TestStringShowsTheTreeStateAlways: a suffix that appears only on a dirty
// build is one a reader has to already know about in order to miss it.
func TestStringShowsTheTreeStateAlways(t *testing.T) {
	for _, state := range []TreeState{TreeClean, TreeDirty, TreeUnknown} {
		s := Info{Version: "v1", Commit: "abc", TreeState: state, Source: SourceLDFlags}.String()
		if !strings.Contains(s, string(state)) {
			t.Errorf("String() = %q, does not mention tree state %q", s, state)
		}
	}
}
