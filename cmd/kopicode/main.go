// Command kopicode is the interactive terminal coding agent.
//
// Nothing is implemented yet. See docs/SLICE-1.md for the scope of the first
// slice and docs/adr/ for the decisions this build starts from.
package main

import (
	"fmt"
	"os"

	"github.com/leejianrong/kopicode/internal/build"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		// The same identity the engine puts on SessionStarted, printed by the
		// same call. Two ways of asking a binary who it is would eventually
		// disagree, and the one nobody looks at would be the one in the record.
		fmt.Println(build.Current())
		return
	}

	// The REPL is slice-1 build step 13. Until it exists, exit non-zero rather
	// than pretending: a zero exit from an unimplemented binary is how a broken
	// harness passes a smoke test.
	fmt.Fprintln(os.Stderr, "kopicode: not implemented yet — see docs/SLICE-1.md")
	os.Exit(4)
}
