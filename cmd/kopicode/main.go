// Command kopicode is the interactive terminal coding agent.
//
// Nothing is implemented yet. See docs/SLICE-1.md for the scope of the first
// slice and docs/adr/ for the decisions this build starts from.
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

	// The REPL is slice-1 build step 13. Until it exists, exit non-zero rather
	// than pretending: a zero exit from an unimplemented binary is how a broken
	// harness passes a smoke test.
	fmt.Fprintln(os.Stderr, "kopicode: not implemented yet — see docs/SLICE-1.md")
	os.Exit(4)
}
