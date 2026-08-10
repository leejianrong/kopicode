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
package arch
