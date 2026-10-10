package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// editorCommand is the person's editor from $VISUAL, then $EDITOR, split into
// words so "code -w" works. It is empty when neither is set.
func editorCommand() []string {
	for _, name := range []string{"VISUAL", "EDITOR"} {
		if f := strings.Fields(os.Getenv(name)); len(f) > 0 {
			return f
		}
	}
	return nil
}

// editorEnv is the environment an editor runs in: the person's own, because an
// editor needs their terminal, home and config, minus every credential kopicode
// itself holds. Not a sandbox: the editor is the person's own program, started
// by them, and the point is only that the provider key is never handed on.
func editorEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "OPENROUTER_API_KEY", "KOPICODE_PROVIDER_API_KEY":
			continue
		}
		// GIT_* overrides would redirect a git-aware editor at another repository.
		if strings.HasPrefix(name, "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// editFile opens path in the person's editor on their terminal and waits.
func editFile(std streams, argv []string, path string) error {
	if len(argv) == 0 {
		return fmt.Errorf("no editor configured")
	}
	cmd := exec.Command(argv[0], append(append([]string(nil), argv[1:]...), path)...)
	cmd.Dir = filepath.Dir(path)
	cmd.Env = editorEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if f, ok := std.in.(*os.File); ok {
		cmd.Stdin = f
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", argv[0], err)
	}
	return nil
}
