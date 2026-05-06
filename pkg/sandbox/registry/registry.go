// Package registry centralises the iterion-shipped sandbox-driver
// constructor list.
//
// pkg/runtime and pkg/cli both need to know which drivers are
// available; before this package existed they each maintained a
// near-identical map literal, with a comment acknowledging the drift
// risk. Hoisting the registry here gives a single source of truth
// without introducing a cli ↔ runtime import cycle (both packages
// already import pkg/sandbox transitively, and this sub-package only
// imports the concrete driver packages).
package registry

import (
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/docker"
	"github.com/SocialGouv/iterion/pkg/sandbox/kubernetes"
	"github.com/SocialGouv/iterion/pkg/sandbox/noop"
)

// Default returns the iterion-shipped sandbox-driver registry. Add
// new drivers here once and both the engine and the doctor command
// pick them up. The factory walks this map per its host-specific
// preference order — see pkg/sandbox.preferenceOrder.
//
// The map is built fresh on every call so callers that want to
// override or extend it (tests, plugins) can mutate without
// affecting other consumers.
func Default() map[string]sandbox.DriverConstructor {
	return map[string]sandbox.DriverConstructor{
		"docker":     docker.Constructor,
		"podman":     docker.Constructor, // same code path; runtime detection picks
		"kubernetes": kubernetes.Constructor,
		"noop":       noop.Constructor,
	}
}
