package bench

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// KOP-234: a task's HOME must not be somewhere the Go toolchain's telemetry can
// keep writing after the run, so the home is made with telemetry already off.
func TestMakeTaskHomeTurnsGoTelemetryOff(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the config directory is %AppData% there, not under HOME")
	}
	home := filepath.Join(t.TempDir(), "home")
	if err := makeTaskHome(home, time.Date(2026, 11, 7, 3, 4, 5, 0, time.UTC)); err != nil {
		t.Fatalf("makeTaskHome: %v", err)
	}
	dir := filepath.Join(home, ".config", "go", "telemetry")
	if runtime.GOOS == "darwin" {
		dir = filepath.Join(home, "Library", "Application Support", "go", "telemetry")
	}
	got, err := os.ReadFile(filepath.Join(dir, "mode"))
	if err != nil {
		t.Fatalf("no telemetry mode file: %v", err)
	}
	if want := "off 2026-11-07"; strings.TrimSpace(string(got)) != want {
		t.Errorf("mode file = %q, want %q (the format `go telemetry off` writes)", got, want)
	}
}

func TestMakeTaskHomeIsPrivateAndRepeatable(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	for range 2 {
		if err := makeTaskHome(home, time.Now()); err != nil {
			t.Fatalf("makeTaskHome: %v", err)
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("home mode = %v, want 0700", info.Mode().Perm())
	}
}
