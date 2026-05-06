// Package proc holds tiny process-management primitives shared across
// iterion's shell-out wrappers (gitCmd, dockerCmd, kubectlCmd).
//
// Today the only export is [DetachProcessGroup], which lifts a child
// process out of its parent's process group so a SIGTERM delivered
// to the parent (typical when `watchexec -r` rebuilds the editor or
// the runner's PGID is signalled by k8s on rolling update) does not
// propagate and kill an in-flight git/docker/kubectl call mid-write.
//
// The build-tagged Unix and Windows variants live alongside this
// file; importers don't need to think about portability.
package proc
