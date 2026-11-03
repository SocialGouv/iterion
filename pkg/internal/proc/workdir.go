package proc

import (
	"os"
	"os/exec"
)

// SetWorkDir points a host spawn at dir and makes PWD agree with it — the
// one gesture every host spawn of an agent CLI crosses.
//
// A child that reads PWD instead of its real working directory would
// otherwise work on the PARENT's checkout: the inherited environment carries
// the engine's PWD, and the spawn only changes the process's cwd. Measured on
// opencode 2.2.5, which resolves its PROJECT from the variable — with PWD
// naming another directory the workspace's own `.opencode/` is never read,
// the agent gets the engine's instructions, and iterion's workspace screen
// guards a tree the CLI never opened. A shell child cannot witness the bug:
// a POSIX shell recomputes PWD from its real cwd at startup, so only a
// non-shell child (the one in workdir_test.go) proves the variable travels.
//
// The override is APPENDED, which is the value os/exec keeps for a duplicate
// key (dedupEnv keeps the last). A nil cmd.Env means "inherit", and appending
// to it would hand the child an environment of exactly one entry — so a nil
// Env is materialised from os.Environ() first, which is what inheriting
// means. An empty dir leaves both alone: the spawn then inherits the
// parent's cwd, and PWD already names it.
//
// PWD is credential-shaped for internal/envtrust (PW/PWD are its password
// segments), so a future gate that screens or redacts a spawn environment
// must not read this working-directory entry as a secret. Nothing does
// today: the entry is appended at spawn time and never reaches LaunchEnv or
// platformcfg.
//
// Sandboxed spawns do NOT cross this helper: a sandbox driver owns the
// child's environment and cwd (docker/kubectl `--workdir` plus the image's
// own PWD), so the gesture belongs to each driver, not to its caller.
func SetWorkDir(cmd *exec.Cmd, dir string) {
	if dir == "" {
		return
	}
	cmd.Dir = dir
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "PWD="+dir)
}
