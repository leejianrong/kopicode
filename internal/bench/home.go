package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// makeTaskHome creates a task's private HOME and turns the Go toolchain's
// telemetry off inside it (KOP-234).
//
// A `go` command started with this HOME records counters under its config
// directory when it exits, and a detached helper it spawns can keep writing there
// after the command the runner waited on has returned. The runner removes the
// task's output directory when the run is over, and a file appearing during that
// removal made it fail with "directory not empty", which surfaced in the paired-run
// test as a TempDir cleanup error. Mode "off" is what `go telemetry off` writes, and
// with it the toolchain writes nothing there. It changes nothing a task measures:
// telemetry is not an input to a build or a test.
func makeTaskHome(home string, now time.Time) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	dir := filepath.Join(home, ".config", "go", "telemetry")
	switch runtime.GOOS {
	case "darwin":
		dir = filepath.Join(home, "Library", "Application Support", "go", "telemetry")
	case "windows":
		return nil // the config directory is %AppData%, not under the HOME the runner sets
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	mode := fmt.Sprintf("off %s", now.UTC().Format("2006-01-02"))
	return os.WriteFile(filepath.Join(dir, "mode"), []byte(mode), 0o600)
}
