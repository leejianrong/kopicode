package harness_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/harness"
)

const allHooks = `pre_tool_hook = ["scripts/guard", "--strict"]
post_tool_hook = ["gofmt", "-l", "."]  # a comment
stop_hook = ["make", "lint"]
hook_tools = ["write_file", "run_shell"]
`

func TestHookKeysAreReadFromTheUserFile(t *testing.T) {
	userHome(t, allHooks)
	sel, err := harness.Resolve(repoWith(t, ""), harness.Overrides{UserConfig: true})
	if err != nil {
		t.Fatal(err)
	}
	want := harness.Hooks{
		PreTool:  []string{"scripts/guard", "--strict"},
		PostTool: []string{"gofmt", "-l", "."},
		Stop:     []string{"make", "lint"},
		Tools:    []string{"write_file", "run_shell"},
	}
	if !reflect.DeepEqual(sel.Config.Hooks, want) {
		t.Errorf("hooks = %+v, want %+v", sel.Config.Hooks, want)
	}
}

func TestEachHookKeyIsReadOnItsOwn(t *testing.T) {
	for key, field := range map[string]func(harness.Hooks) []string{
		"pre_tool_hook":  func(h harness.Hooks) []string { return h.PreTool },
		"post_tool_hook": func(h harness.Hooks) []string { return h.PostTool },
		"stop_hook":      func(h harness.Hooks) []string { return h.Stop },
	} {
		userHome(t, key+" = [\"echo\", \"a b\"]\n")
		sel, err := harness.Resolve(repoWith(t, ""), harness.Overrides{UserConfig: true})
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if got := field(sel.Config.Hooks); !reflect.DeepEqual(got, []string{"echo", "a b"}) {
			t.Errorf("%s = %q", key, got)
		}
		if !sel.Config.Hooks.Any() || len(sel.Config.Hooks.Tools) != 0 {
			t.Errorf("%s: hooks = %+v; with no hook_tools every tool applies", key, sel.Config.Hooks)
		}
	}
}

func TestMalformedHookKeysAreUsageErrors(t *testing.T) {
	for name, tc := range map[string]struct{ line, want string }{
		"a shell string":     {`pre_tool_hook = "make lint"`, "not an array"},
		"an empty argv":      {`stop_hook = []`, "is empty"},
		"an empty program":   {`post_tool_hook = [" ", "x"]`, "names no program"},
		"an empty tool list": {`hook_tools = []`, "is empty"},
		"an unknown tool":    {`hook_tools = ["write_fle"]`, "not a tool"},
		"a bare string":      {`hook_tools = "write_file"`, "not an array"},
	} {
		userHome(t, tc.line+"\n")
		_, err := harness.Resolve(repoWith(t, ""), harness.Overrides{UserConfig: true})
		if err == nil || !harness.IsUsageError(err) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want a usage error containing %q", name, err, tc.want)
		}
	}
}

func TestARepositoryCannotDeclareAHook(t *testing.T) {
	for _, key := range []string{
		`pre_tool_hook = ["x"]`, `post_tool_hook = ["x"]`, `stop_hook = ["x"]`, `hook_tools = ["run_shell"]`, `hook_timeout_seconds = 60`,
	} {
		userHome(t, "")
		repo := repoWith(t, key+"\n")
		for _, o := range []harness.Overrides{{}, {UserConfig: true}} {
			_, err := harness.Resolve(repo, o)
			if err == nil || !harness.IsUsageError(err) ||
				!strings.Contains(err.Error(), "belongs in the user config") || !strings.Contains(err.Error(), "ADR-0036") {
				t.Errorf("%s in a repository (user=%v): err = %v", key, o.UserConfig, err)
			}
		}
	}
}

func TestOnlyTheUserReadingFrontEndGetsHooks(t *testing.T) {
	userHome(t, allHooks)
	sel, err := harness.Resolve(repoWith(t, ""), harness.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Config.Hooks.Any() {
		t.Errorf("run --print, serve, mcp and kopibench resolved hooks: %+v", sel.Config.Hooks)
	}
}

func TestHooksMoveTheHashOnlyWhenSet(t *testing.T) {
	cfg, ok := harness.ConfigByName(harness.DefaultConfigName)
	if !ok {
		t.Fatal("no default config")
	}
	if cfg.Hooks.Any() {
		t.Fatal("a registered arm has hooks; every benchmark arm must run none")
	}
	off := cfg.Hash()

	cfg.Hooks = harness.Hooks{PreTool: []string{"guard"}}
	pre := cfg.Hash()
	cfg.Hooks = harness.Hooks{PostTool: []string{"guard"}}
	post := cfg.Hash()
	cfg.Hooks = harness.Hooks{Stop: []string{"guard"}}
	stop := cfg.Hash()
	cfg.Hooks = harness.Hooks{PreTool: []string{"guard"}, Tools: []string{"run_shell"}}
	scoped := cfg.Hash()
	seen := map[string]string{off: "none"}
	for name, h := range map[string]string{"pre": pre, "post": post, "stop": stop, "scoped": scoped} {
		if prev, dup := seen[h]; dup {
			t.Errorf("%s shares a hash with %s", name, prev)
		}
		seen[h] = name
	}

	cfg.Hooks = harness.Hooks{}
	if cfg.Hash() != off {
		t.Error("clearing the hooks did not restore the original hash")
	}
}

// hook_tools with no command does nothing, and Resolve must not let it move the
// hash.
func TestHookToolsWithNoCommandLeavesTheHashAlone(t *testing.T) {
	userHome(t, "hook_tools = [\"run_shell\"]\n")
	sel, err := harness.Resolve(repoWith(t, ""), harness.Overrides{UserConfig: true})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := harness.ConfigByName(sel.Config.Name)
	if sel.HarnessConfigHash != base.Hash() {
		t.Error("hook_tools alone moved the hash")
	}
}

func TestASessionWithNoHooksHashesAsBefore(t *testing.T) {
	userHome(t, "model = \"minimax/minimax-m2\"\n")
	repo := repoWith(t, "")
	plain, err := harness.Resolve(repo, harness.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	withUser, err := harness.Resolve(repo, harness.Overrides{UserConfig: true})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := harness.ConfigByName(plain.Config.Name)
	if plain.HarnessConfigHash != base.Hash() || withUser.Config.Hooks.Any() {
		t.Error("a session with no hooks hashes differently from the registered config")
	}
	userHome(t, allHooks)
	hooked, err := harness.Resolve(repo, harness.Overrides{UserConfig: true})
	if err != nil {
		t.Fatal(err)
	}
	if hooked.HarnessConfigHash == plain.HarnessConfigHash {
		t.Error("a hooked session shares the hash of an unhooked one")
	}
}

func TestHookTimeoutSecondsIsReadFromTheUserFile(t *testing.T) {
	for _, tc := range []struct {
		line string
		want int
	}{
		{"hook_timeout_seconds = 60", 60},
		{"hook_timeout_seconds = 1", 1},
		{"hook_timeout_seconds = 300  # five minutes, the ceiling", 300},
	} {
		userHome(t, "stop_hook = [\"make\", \"lint\"]\n"+tc.line+"\n")
		sel, err := harness.Resolve(repoWith(t, ""), harness.Overrides{UserConfig: true})
		if err != nil {
			t.Fatalf("%s: %v", tc.line, err)
		}
		if got := sel.Config.Hooks.TimeoutSeconds; got != tc.want {
			t.Errorf("%s: TimeoutSeconds = %d, want %d", tc.line, got, tc.want)
		}
	}
}

func TestMalformedHookTimeoutsAreUsageErrors(t *testing.T) {
	for name, tc := range map[string]struct{ line, want string }{
		"zero":              {`hook_timeout_seconds = 0`, "out of range"},
		"one over the cap":  {`hook_timeout_seconds = 301`, "out of range"},
		"negative":          {`hook_timeout_seconds = -5`, "out of range"},
		"a quoted number":   {`hook_timeout_seconds = "30"`, "not an integer"},
		"a float":           {`hook_timeout_seconds = 1.5`, "not an integer"},
		"a word":            {`hook_timeout_seconds = soon`, "not an integer"},
		"text after number": {`hook_timeout_seconds = 30 seconds`, "not an integer"},
		"no value":          {`hook_timeout_seconds =`, "has no value"},
	} {
		userHome(t, tc.line+"\n")
		_, err := harness.Resolve(repoWith(t, ""), harness.Overrides{UserConfig: true})
		if err == nil || !harness.IsUsageError(err) || !strings.Contains(err.Error(), tc.want) ||
			!strings.Contains(err.Error(), "config.toml:1") {
			t.Errorf("%s: err = %v, want a usage error naming the file and line, containing %q", name, err, tc.want)
		}
	}
}

func TestAMisspeltHookTimeoutKeyIsNotIgnored(t *testing.T) {
	userHome(t, "hook_timeout_second = 60\n")
	_, err := harness.Resolve(repoWith(t, ""), harness.Overrides{UserConfig: true})
	if err == nil || !strings.Contains(err.Error(), `did you mean "hook_timeout_seconds"`) {
		t.Errorf("err = %v", err)
	}
}

func TestOnlyTheUserReadingFrontEndGetsAHookTimeout(t *testing.T) {
	userHome(t, "stop_hook = [\"make\"]\nhook_timeout_seconds = 90\n")
	sel, err := harness.Resolve(repoWith(t, ""), harness.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Config.Hooks.TimeoutSeconds != 0 {
		t.Errorf("run --print, serve, mcp and kopibench resolved a hook timeout: %+v", sel.Config.Hooks)
	}
}

func TestAHookTimeoutMovesTheHashOnlyWhenSet(t *testing.T) {
	cfg, _ := harness.ConfigByName(harness.DefaultConfigName)
	cfg.Hooks = harness.Hooks{Stop: []string{"make", "lint"}}
	unset := cfg.Hash()
	cfg.Hooks.TimeoutSeconds = 60
	sixty := cfg.Hash()
	cfg.Hooks.TimeoutSeconds = 61
	sixtyOne := cfg.Hash()
	if unset == sixty || sixty == sixtyOne || unset == sixtyOne {
		t.Errorf("a hook timeout did not move the hash: %s %s %s", unset, sixty, sixtyOne)
	}
	cfg.Hooks.TimeoutSeconds = 0
	if cfg.Hash() != unset {
		t.Error("clearing the timeout did not restore the hash a hooked session had without one")
	}
}

// A timeout with no command does nothing, and Resolve must not let it move the
// hash or appear in the session's hooks.
func TestAHookTimeoutWithNoCommandLeavesTheHashAlone(t *testing.T) {
	userHome(t, "hook_timeout_seconds = 90\n")
	sel, err := harness.Resolve(repoWith(t, ""), harness.Overrides{UserConfig: true})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := harness.ConfigByName(sel.Config.Name)
	if sel.HarnessConfigHash != base.Hash() || sel.Config.Hooks.TimeoutSeconds != 0 {
		t.Errorf("hook_timeout_seconds alone moved the hash or set hooks: %+v", sel.Config.Hooks)
	}
}
