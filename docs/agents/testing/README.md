# Testing — the domain

| Leaf | Read it when |
|---|---|
| [testing.md](testing.md) | Adding a test, choosing the proof layer, generating the capability census, a leaking test, process-stream captures and sweep roots. |
| [flakes.md](flakes.md) | A result depends on the machine: evidence, disposition, dated re-checks (owned Pi config, private OpenAPI across worktrees, virtual-time drains, Git registry writers). |
| [host-shaped-witnesses.md](host-shaped-witnesses.md) | Re-auditing the seven host-sensitive packages: signals, wall-clock bounds, the Kubernetes pod-mount witness. |
| [deployment-posture-bench.md](deployment-posture-bench.md) | A guarantee the deployment decides, not the code: the shipped image under the real securityContext, the four answers `landlock_create_ruleset` can give, and why the seccomp MODE is not the profile. |
| [posture-bench-remote.md](posture-bench-remote.md) | Running the posture bench against a cluster this host does not own: the bench image built from the deployment's own digest, the kubectl driver, the runtime it attributes and the control it does not earn (`task bench:codemode-bench-image`, `bench:codemode-remote`). |
| [native-ceiling.md](native-ceiling.md) | The memory bound `RLIMIT_DATA` does not reach (a `MAP_SHARED`/memfd payload): the parent's resident-set probe, `oom_score_adj`, the overshoot it admits, and the in-pod leg still owed. |
| [posture-bench-mutants.md](posture-bench-mutants.md) | The posture bench's mutant register: what each one removes, what it made red, and the one assumed survivor. Adding a guard there means adding its mutant here. |
| [engine-contracts.md](engine-contracts.md) | A bot delegates to an external CLI (ultra11y for rgaa-audit): pinning it, testing its contract on the real binary, bumping the pin, a `known_upstream_defect_…` subtest going red. |
| [process-fixtures.md](process-fixtures.md) | Captures, sweep roots, chart scratch, PID publication, corpus copies, command-test HOME, installed binaries, recursive tests and foreign host state. |
| [load-margins.md](load-margins.md) | A wall-clock bound blew on a loaded runner: re-pricing it by ratio or explicit margin, never a guessed constant. |
| [background-oracles.md](background-oracles.md) | Proving an immediate background-session end independently of CLI startup, and distinguishing it from budget assertions. |
| [boundary-oracles.md](boundary-oracles.md) | Redaction bounds, fmt deadlines, Mongo/watch, leases, parser work, React titles, sign-out retries and pricing isolation. |
