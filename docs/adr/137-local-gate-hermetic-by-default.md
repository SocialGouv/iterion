# ADR-137 — The local gate is hermetic by default

- **Status**: Proposed (2026-10-24; implementation and review in #2585)
- **Date**: 2026-10-24
- **Deciders**: jo (gate posture), Codex (implementation)
- **Serves**: [#2585](https://github.com/SocialGouv/iterion/issues/2585)

## Context

An unrelated diff's `task check` spends the full 1200-second package timeout
in `pkg/sandbox/kubernetes`. At the cutoff, `os/exec` writer goroutines remain
alive: an inherited pipe can outlive the process being waited for. The same
package finishes in about seven seconds in a jail. These two sandbox outcomes
are evidence of machine-dependent execution, not proof that the gate needs
network access.

The download hypothesis is refuted at baseline `e40def071`: the HTTPS URL in
`workspace_test.go` is written into a credential file, never dialled. Jetify
denials in the gate log belong to devbox setup. The remaining classes are a
fake kubectl whose sleeping descendants inherit pipes, and host Git/shell
configuration or askpass helpers entering tests that do not own HOME. The dump
does not establish which suspect caused the original hang.

## Options

1. **Skip on the test side.** Cheap relief, but the asserted contract becomes
   machine-dependent and a missing proof reads as green. Rejected as a repair.
2. **Open the gate's network.** It changes the environment without explaining
   the surviving writers, permits accidental credentials/egress and contradicts
   the refuted download diagnosis. Rejected as the default.
3. **Isolate the exec class.** Inventory direct subprocess calls and the
   production/helper paths that spawn them, share HOME/Git isolation, own
   fixture children and enforce an independent test bound. Chosen.

## Decision

- The deterministic local gate is **hermetic by default**. Egress remains
  denied in gate runs. A test requiring a real external service has an explicit,
  named opt-in identifying the test and destination; it never opens the whole
  gate implicitly. Dependency provisioning is a separate, visible setup step.
- Tests executing binaries use owned scratch and HOME, empty GIT_ASKPASS and
  SSH_ASKPASS, `GIT_CONFIG_NOSYSTEM=1` and an empty scratch global config through
  one shared helper. Git fixture helpers keep auto-maintenance disabled.
- Process fixtures own their children through completion; cancellation and
  failed assertions do not leave pipe holders behind. Production WaitDelay
  remains asserted, with an independent hard bound on its witness.
- Missing capabilities and unavailable prerequisites produce **named, fast
  refusals**, and the report names what remains unverified. An unsupported
  machine is not silently counted as proof.
- A machine-dependent test is **annotated and measured**, never deleted or
  silently made green. The [flake register](../agents/testing/flakes.md) records
  its test, machine class, log/SHA evidence, disposition and re-check date.

## Consequences

The [push-to-main runbook](../agents/workflow/push-to-main.md) separates setup,
local proof, review, train and post-push CI witnesses. A saturated ARC queue is
an unavailable witness, not a successful test. A clean jailed run does not
erase a workstation failure: both environments and timings belong in evidence.

Isolation is paid once per class and exercised through existing contracts.
Removing a production bound must make its witness fail on its own assertion
within the test's independent budget. This decision does not claim that
`task check` itself installs a network sandbox: the gate launcher enforces the
egress policy. Live tests and setup downloads retain their named opt-ins.
