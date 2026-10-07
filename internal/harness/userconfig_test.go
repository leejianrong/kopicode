package harness_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/harness"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// userHome points the user config at a temp directory and returns it.
func userHome(t *testing.T, config string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KOPICODE_HOME", home)
	if config != "" {
		writeFile(t, filepath.Join(home, "config.toml"), config)
	}
	return home
}

func repoWith(t *testing.T, config string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if config != "" {
		writeFile(t, filepath.Join(dir, ".kopicode", "config.toml"), config)
	}
	return dir
}

func TestUserConfigDirPrecedence(t *testing.T) {
	t.Setenv("KOPICODE_HOME", "/k")
	t.Setenv("XDG_CONFIG_HOME", "/x")
	if d := harness.UserConfigDir(); d != "/k" {
		t.Errorf("KOPICODE_HOME must win, got %s", d)
	}
	t.Setenv("KOPICODE_HOME", "")
	if d := harness.UserConfigDir(); d != "/x/kopicode" {
		t.Errorf("XDG_CONFIG_HOME next, got %s", d)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/h")
	if d := harness.UserConfigDir(); d != "/h/.config/kopicode" {
		t.Errorf("home default, got %s", d)
	}
}

func TestUserConfigIsReadOnlyWhenAFrontEndAsks(t *testing.T) {
	userHome(t, "model = \"minimax/minimax-m2\"\ndefault_mode = \"auto\"\nauto_never_allow = [\"terraform apply\"]\n")
	dir := repoWith(t, "")

	plain, err := harness.Resolve(dir, harness.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if plain.ModelID != harness.DefaultModelID || plain.Settings.DefaultMode != "" || len(plain.Settings.AutoNeverAllow) != 0 {
		t.Fatalf("serve/bench resolution read the user file: %+v", plain.Settings)
	}

	sel, err := harness.Resolve(dir, harness.Overrides{UserConfig: true})
	if err != nil {
		t.Fatal(err)
	}
	if sel.ModelID != "minimax/minimax-m2" || sel.ModelSource != harness.SourceUser {
		t.Errorf("model = %s from %s, want the user file's", sel.ModelID, sel.ModelSource)
	}
	if sel.Settings.DefaultMode != "auto" || len(sel.Settings.AutoNeverAllow) != 1 {
		t.Errorf("settings = %+v", sel.Settings)
	}
}

func TestModelPrecedenceIsFlagThenRepoThenUserThenDefault(t *testing.T) {
	userHome(t, "model = \"minimax/minimax-m2\"\n")
	repo := repoWith(t, "model = \"qwen/qwen3-coder-next\"\n")
	o := harness.Overrides{UserConfig: true}
	if sel, _ := harness.Resolve(repo, o); sel.ModelID != "qwen/qwen3-coder-next" || sel.ModelSource != harness.SourceFile {
		t.Errorf("repo must beat user: %s from %s", sel.ModelID, sel.ModelSource)
	}
	o.Model = "minimax/minimax-m2"
	if sel, _ := harness.Resolve(repo, o); sel.ModelSource != harness.SourceFlag {
		t.Errorf("flag must beat both: from %s", sel.ModelSource)
	}
}

func TestNeverAllowEntriesAddUpAcrossFiles(t *testing.T) {
	userHome(t, "auto_never_allow = [\"terraform apply\", \"kubectl delete\"]\n")
	repo := repoWith(t, "auto_never_allow = [\"make deploy\"]\n")
	sel, err := harness.Resolve(repo, harness.Overrides{UserConfig: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sel.Settings.AutoNeverAllow, "|"); got != "terraform apply|kubectl delete|make deploy" {
		t.Errorf("never-allow = %s; a file may only lengthen the list", got)
	}
	// The repository's own entries apply even where the user file is not read.
	sel, _ = harness.Resolve(repo, harness.Overrides{})
	if len(sel.Settings.AutoNeverAllow) != 1 {
		t.Errorf("repo entries must apply everywhere: %v", sel.Settings.AutoNeverAllow)
	}
}

func TestARepositoryCannotChooseTheModeOrSkillPaths(t *testing.T) {
	for _, key := range []string{`default_mode = "auto"`, `skills_paths = ["/x"]`} {
		repo := repoWith(t, key+"\n")
		_, err := harness.Resolve(repo, harness.Overrides{})
		if err == nil || !harness.IsUsageError(err) || !strings.Contains(err.Error(), "belongs in the user config") {
			t.Errorf("%s in a repository: err = %v", key, err)
		}
	}
}

func TestPerRepositoryKeysAreRefusedInTheUserFile(t *testing.T) {
	for _, key := range []string{`harness = "default"`, `verify = ["make"]`} {
		userHome(t, key+"\n")
		_, err := harness.Resolve(repoWith(t, ""), harness.Overrides{UserConfig: true})
		if err == nil || !strings.Contains(err.Error(), "per-repository key") {
			t.Errorf("%s in the user file: err = %v", key, err)
		}
	}
}

func TestUserConfigErrorsNameTheFile(t *testing.T) {
	home := userHome(t, "default_mode = \"plan\"\n")
	_, err := harness.Resolve(repoWith(t, ""), harness.Overrides{UserConfig: true})
	if err == nil || !strings.Contains(err.Error(), filepath.Join(home, "config.toml")+":1") {
		t.Errorf("err = %v, want file:line", err)
	}
}

func TestUserAgentsPath(t *testing.T) {
	home := userHome(t, "")
	if p := harness.UserAgentsPath(); p != "" {
		t.Errorf("no file, got %q", p)
	}
	writeFile(t, filepath.Join(home, "AGENTS.md"), "be terse")
	if p := harness.UserAgentsPath(); p != filepath.Join(home, "AGENTS.md") {
		t.Errorf("got %q", p)
	}
}

func TestAnUnknownUserModelNamesTheUserFile(t *testing.T) {
	home := userHome(t, "model = \"nope/none\"\n")
	_, err := harness.Resolve(repoWith(t, ""), harness.Overrides{UserConfig: true})
	if err == nil || !strings.Contains(err.Error(), filepath.Join(home, "config.toml")) {
		t.Errorf("err = %v, want the user file named", err)
	}
}

func TestATypoedKeyIsRefusedNotIgnored(t *testing.T) {
	userHome(t, "defaul_mode = \"auto\"\n")
	_, err := harness.Resolve(repoWith(t, ""), harness.Overrides{UserConfig: true})
	if err == nil || !strings.Contains(err.Error(), `did you mean "default_mode"`) {
		t.Errorf("err = %v, want a did-you-mean", err)
	}
	// A neighbour's key that is nothing like ours is still skipped.
	userHome(t, "colour = \"red\"\neditor_theme = \"dark\"\n")
	if _, err := harness.Resolve(repoWith(t, ""), harness.Overrides{UserConfig: true}); err != nil {
		t.Errorf("an unrelated key must be ignored: %v", err)
	}
}

func TestAnEmptyNeverAllowEntryNamesTheLine(t *testing.T) {
	home := userHome(t, "model = \"qwen/qwen3-coder-next\"\nauto_never_allow = [\"\"]\n")
	_, err := harness.Resolve(repoWith(t, ""), harness.Overrides{UserConfig: true})
	if err == nil || !strings.Contains(err.Error(), filepath.Join(home, "config.toml")+":2") {
		t.Errorf("err = %v, want file:2", err)
	}
}

func TestAByteOrderMarkIsNotPartOfTheFirstKey(t *testing.T) {
	userHome(t, "\xef\xbb\xbfmodel = \"minimax/minimax-m2\"\n")
	sel, err := harness.Resolve(repoWith(t, ""), harness.Overrides{UserConfig: true})
	if err != nil || sel.ModelID != "minimax/minimax-m2" {
		t.Errorf("model = %q, err = %v", sel.ModelID, err)
	}
}

func TestSkillsPathsRefusalGivesItsOwnReason(t *testing.T) {
	_, err := harness.Resolve(repoWith(t, "skills_paths = [\"/x\"]\n"), harness.Overrides{})
	if err == nil || strings.Contains(err.Error(), "visitors are asked") || !strings.Contains(err.Error(), "outside itself") {
		t.Errorf("err = %v", err)
	}
}
