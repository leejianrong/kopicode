package main

// The process exit codes, in one place because two front ends and a bench
// harness read them and a second copy is how they stop agreeing.
//
// The enumeration is docs/SLICE-1.md §Build Plan step 14, and
// engine.Stop.ExitCode is the mapping from a loop outcome onto it — so an
// outcome the engine settled on is never re-decided here. What this file owns
// is the two codes the engine cannot produce: a usage error, which happens
// before an engine exists (ADR-0007 decision 4), and the code a surface
// failure reports.
//
//	0  success
//	1  the task was not completed — the loop ran cleanly and did not finish
//	2  usage error — an unknown model or harness, a bad flag; nothing was
//	   opened, locked or written
//	3  provider error — a call that produced no usable reply after the
//	   client's own retries
//	4  harness error — kopicode itself broke, and the fail-closed default for
//	   any outcome nobody mapped
const (
	exitSuccess      = 0
	exitNotCompleted = 1
	exitUsage        = 2
	exitProvider     = 3
	exitHarness      = 4
)
