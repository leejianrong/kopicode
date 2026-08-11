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

	"github.com/leejianrong/kopicode/internal/build"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		// The identity a bench result is attributed to. Printing it from the
		// same call the record uses is what keeps a report and a journal from
		// disagreeing about which binary produced a number.
		fmt.Println(build.Current())
		return
	}

	fmt.Fprintln(os.Stderr, "kopibench: not implemented yet — see docs/SLICE-1.md")
	os.Exit(4)
}
