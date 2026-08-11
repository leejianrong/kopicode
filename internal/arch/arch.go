// Package arch holds the architectural boundary guards.
//
// It contains no runtime code. Its tests enforce the boundary that
// docs/adr/0003-single-repo-internal-engine.md decides but that nothing else
// can enforce: Go's internal/ rule stops code *outside* this module from
// importing the engine, and the compiler has nothing to say about which
// internal packages the front ends reach into.
//
// That makes this test the only thing holding a boundary with no network edge
// and no module edge behind it. Per the dev-playbook, a guard is unproven until
// it has been seen to fail — so if you change it, break it on purpose first and
// confirm the failure names the right package.
//
// gitcmd_test.go holds a second guard of the same shape but a different
// subject: every git subprocess in this repository must name its target
// directory and build its own environment. That one is not an architectural
// boundary, it is an incident that already happened, and it lives here because
// this is where the repository keeps its static checks over its own source.
package arch
