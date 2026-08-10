// Command kopibench is the headless benchmark runner.
//
// It is a first-class front end, not a test utility: a benchmark cannot drive a
// REPL, which is why the engine has two surfaces from the first commit
// (docs/adr/0003-single-repo-internal-engine.md).
//
// Nothing is implemented yet. See docs/SLICE-1.md build steps 15-17.
package main

import (
	"fmt"
	"os"
)

// version is overridden at build time via -ldflags.
var version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println(version)
		return
	}

	fmt.Fprintln(os.Stderr, "kopibench: not implemented yet — see docs/SLICE-1.md")
	os.Exit(4)
}
