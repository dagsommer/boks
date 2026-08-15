// Package release holds no code. It exists for the tests beside it, which check that the
// several places describing what a release ships still describe the same release.
//
// Boks states the shipped platform set three times — the Makefile's RELEASE_TARGETS so a
// release can be reproduced off GitHub, the build matrix in .github/workflows/release.yml so
// CI produces it, and prose in both saying which platforms have booted a sandbox. Three
// statements of one fact drift, and this one drifts silently: `make dist` quietly building a
// different set than the release does is not visible until someone compares two directories.
//
// A test rather than generation. Generating the matrix from the Makefile would need a
// generator in CI and buys less than it costs, and neither file is edited often. What matters
// is that editing one and not the other fails.
package release
