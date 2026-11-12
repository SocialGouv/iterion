# Kubernetes execution environment transport

A Kubernetes command with a nonempty per-call `ExecOpts.Env` uses the native
capsule/guest pair. Overrides are invocation data; they do not become host
`kubectl` configuration or shell text in its arguments. The capsule captures the
actual pod process environment on each invocation, before the guest applies the
overrides. An empty override map retains the historical command path.

The host is the real `iterion __exec-env-host-v1` entry, dispatched before application
initialization. The guest is a separate Go leaf with only standard-library and
protocol dependencies. Its native capsule has no libc or dynamic loader. Both
image files and their traversed parents must be non-writable by the workload.
Old or custom images lacking a compatible pair refuse the invocation; the driver
does not inject binaries, download a helper, or silently switch sandbox drivers.

## Operator configuration

`New` captures these `ITERION_SANDBOX_K8S_EXEC_` settings once. Repository-planted
values refuse. Homonymous workload variables remain target data.

| Suffix | Default and contract |
|---|---|
| `HOST_HELPER` | Trusted `iterion` lookup; an explicit path must be absolute. |
| `GUEST_HELPER` | `/usr/local/libexec/iterion-execenv-capsule`; an absolute path to a compatible image pair. |
| `PROTOCOL` | `2`; explicit unsupported versions refuse, with no downgrade. |
| `DEFAULT_PATH` | `/bin:/usr/bin`; used only when the target has no PATH. Explicit empty and relative components are preserved. |
| `SHELL` | `/bin/sh`; absolute shell used for ENOEXEC fallback. Target SHELL is not authority. |
| `START_TIMEOUT` | `30s`; `0` is the explicit unbounded override with a warning. |

Caller stdin, including an open `StdinPipe`, remains user data. Control frames
use a separate host envelope. Large shell scripts use the existing streaming
seam; a caller that reserves stdin keeps that reservation. Context cancellation
bounds the local client process group; this is not a guarantee that a remote
Kubernetes workload terminated after a disconnected stream.

## Images and executable proofs

The app Dockerfile builds a static guest and a target-architecture native
capsule. Finalized sandbox images copy the same pair from the selected app image.
`sandbox/finalize/execenv-smoke.sh` runs after the final non-root USER in both
Dockerfiles. Its v2 frame is pinned by the production encoder test, and checks
readiness, target environment, cwd and stdin byte-for-byte. The native image
qualification workflow builds amd64 and ARM on their respective native runners,
then exercises all five finalized variants without publishing them.

The portable Kubernetes tests build the actual CLI and pair from the checkout.
They need a C compiler and bubblewrap on Linux amd64/arm64. Devbox supplies the
Linux isolation tool. CI unit/race jobs use a namespace-capable VM by default,
install Bubblewrap, and execute a read-only mount preflight before the suite.
ARC pods refused that mount in PR #3189 despite having the binary installed.
When Helm is installed, these jobs prepare the declared chart dependencies
before testing; a VM-provided Helm otherwise executes render tests against
missing archives. Repository-discovery diagnostics also differ with Git 2.56:
a dangling gitfile is classified through the shared Git diagnostic helper,
while authoring still refuses a surviving staged index and conservative
cleanup preserves orphan worktrees. Diagnostic text embedded in a path must
never turn an unreadable repository into a deletable orphan; the worktree
classifier matches leading Git diagnostics, not substrings of filenames.
The HTTP scratch-consent fixture explicitly uses `sandbox: none` so this
consent proof does not depend on a VM pulling an unrelated sandbox image.
`CI_NATIVE_EXEC_RUNNER` can select a qualified private runner; the global
`CI_SELF_HOSTED=off` and fork/dependency-bot guards still force GitHub-hosted
runners. This changes no cluster security profile. An absent tool
on a supported platform is a failed proof, not a silent skip.
`ITERION_TEST_BWRAP` may select an explicit test-only bubblewrap executable.
Tests mount the pair read-only under `/opt` in a private namespace; they do not
install files into the operator's image paths. A fixture's mount is not evidence
that published images or a real Kubernetes cluster have been qualified.

Release still owes native images, the real cluster/caller witnesses, a converged
local review and the required gates. Cross-built ARM ELF alone is insufficient.

The Kubernetes command owns its local cancellation hook. SDK adapters preserve
that declared lifecycle, including through child sandbox decorators; unmarked
drivers retain SDK process-group hardening. This does not kill a remote workload.
