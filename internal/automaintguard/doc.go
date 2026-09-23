// Package automaintguard holds the acceptance witness of
// scripts/auto-maintenance-check.sh — the preflight that decides whether a
// repository's dependency loop is safe to run unattended.
//
// The script compares settings that live in three different systems, so
// nothing in any one of them can type-check the comparison. The witness is the
// substitute: every scenario here is a CONFIGURATION DEFECT the script must
// redden on, plus one conforming configuration it must pass. The defects are
// the ones this project actually paid — the gate-context rename applied to one
// of two sites (#1591) and the delivery App that cannot touch the files
// Renovate bumps (#1595) — replayed against fixtures rather than against a
// live forge.
//
// The conforming scenario is not decoration. A bench made only of defects is
// indistinguishable from a script that always exits non-zero, and a guard that
// cannot be green is not a guard.
package automaintguard
