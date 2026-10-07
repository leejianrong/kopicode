package harness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// UserConfigDir is where the user's own kopicode files live (ADR-0024):
// $KOPICODE_HOME if set, else $XDG_CONFIG_HOME/kopicode, else ~/.config/kopicode.
//
// The environment variables only *locate* the directory. What is in it is a
// file, and the values it supplies are resolved and recorded in the journal like
// any other, so ADR-0007 decision 3's concern (an invisible ambient value
// changing the arm) is met by recording, not by refusing to look.
//
// It is "" when there is no home directory to look in (a container with no
// HOME), which means there is no user config rather than a failure.
func UserConfigDir() string {
	if d := os.Getenv("KOPICODE_HOME"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "kopicode")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "kopicode")
}

// UserAgentsPath is the user-level AGENTS.md, or "" when the directory cannot be
// located or the file is absent.
func UserAgentsPath() string {
	dir := UserConfigDir()
	if dir == "" {
		return ""
	}
	p := filepath.Join(dir, AgentsFileName)
	if info, err := os.Stat(p); err != nil || info.IsDir() {
		return ""
	}
	return p
}

// LoadUserConfig reads config.toml from [UserConfigDir]. A missing file is the
// ordinary case and yields the zero [FileConfig]; an unreadable or malformed one
// is an error naming the file and line.
func LoadUserConfig() (FileConfig, error) {
	dir := UserConfigDir()
	if dir == "" {
		return FileConfig{}, nil
	}
	path := filepath.Join(dir, ConfigFileName)
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return FileConfig{}, nil
	case err != nil:
		return FileConfig{}, fmt.Errorf("harness: reading %s: %w", path, err)
	}
	return parseFileConfig(path, string(data), true)
}
