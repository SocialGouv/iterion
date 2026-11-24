// Package proc holds tiny process-management primitives shared across
// iterion's shell-out wrappers (gitCmd, dockerCmd, kubectlCmd, tool
// nodes, hook and guard subprocesses).
//
// Two primitives, both built on Setpgid, with OPPOSITE intents — pick by
// asking who the subprocess must outlive:
//
//   - [DetachProcessGroup] — the child must SURVIVE a signal aimed at the
//     parent's group (`watchexec -r` rebuilding the studio, k8s signalling
//     the runner's PGID on a rolling update). It isolates and never signals.
//
//   - [TerminateGroupOnCancel] — the child's work must END with the caller's
//     context. It isolates AND signals: on cancellation the whole group is
//     SIGKILLed, so a grandchild the child spawned dies with it instead of
//     surviving and holding the inherited output pipes.
//
//   - [KillProcessGroup] — the same signal, dispatched by the CALLER at the
//     moment the wait on the child ends, for the window TerminateGroupOnCancel
//     cannot cover: os/exec's context watcher hands its verdict to Wait as
//     soon as the direct child is reaped, so a Cancel armed on the context is
//     never dispatched once the shell has exited, whatever the context does
//     afterwards. The group id is the child's own pid, recorded at spawn.
//
// Setpgid alone gives neither guarantee to the descendants: it only makes
// the group addressable. A cancellation path that stops at Setpgid stops
// the WAIT, not the work.
//
// Neither primitive reaches what a finished child LEFT holding its output
// pipes, which is the third thing every shell-out here shares:
// [SetWaitDelay] is the one bound on that drain and [WaitDelayEnv] the one
// override for it, [DrainCutShort] the one reader of the sentinel it
// produces.
//
// The build-tagged Unix and Windows variants live alongside this file;
// importers don't need to think about portability.
package proc
