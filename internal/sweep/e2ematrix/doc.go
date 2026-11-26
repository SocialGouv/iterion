// Package e2ematrix holds the guards over docs/e2e-coverage-matrix.md and the
// stabilisation plan derived from it: the e2e-coverage bot's own contract
// code, sliced out of its verify_run tool command, runs over the committed
// matrix, and the plan's first table is recounted from that matrix.
//
// These tests read the whole tree — every citation the matrix makes is opened
// and its test name searched with git grep — so a change anywhere may reach
// them. They live outside bots so that a test selector does not have to run
// the whole bots package for every change in order to run them.
package e2ematrix
