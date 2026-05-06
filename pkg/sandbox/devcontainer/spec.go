package devcontainer

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// ToSandboxSpec converts a parsed devcontainer.json into the
// driver-agnostic [sandbox.Spec] iterion's runtime consumes.
//
// Field mapping:
//
//	image           -> Spec.Image
//	build           -> Spec.Build (Phase 2 deferred at driver layer)
//	containerEnv    -> Spec.Env
//	remoteEnv       -> merged into Spec.Env (containerEnv wins on collision)
//	mounts          -> Spec.Mounts
//	remoteUser      -> Spec.User (preferred)
//	containerUser   -> Spec.User (fallback when remoteUser empty)
//	workspaceFolder -> Spec.WorkspaceFolder
//	postCreateCommand -> Spec.PostCreate (joined to a shell snippet)
//
// The returned Spec has Mode=ModeAuto so callers know it came from
// devcontainer.json reading (vs ModeInline which signals an in-DSL
// block). Network is left nil — iterion handles network policy via
// its own DSL fields, not via devcontainer (which has no equivalent).
func ToSandboxSpec(f *File) sandbox.Spec {
	if f == nil {
		return sandbox.Spec{}
	}
	spec := sandbox.Spec{
		Mode:            sandbox.ModeAuto,
		Image:           f.Image,
		WorkspaceFolder: f.WorkspaceFolder,
	}
	if f.Build != nil {
		spec.Build = &sandbox.Build{
			Dockerfile: f.Build.Dockerfile,
			Context:    f.Build.Context,
			Args:       f.Build.Args,
		}
	}

	// Merge remoteEnv first, then containerEnv (containerEnv wins on
	// collision because it is the spec-canonical field). Both maps may
	// be nil — append to a fresh map and the zero-len cases are no-ops.
	if len(f.RemoteEnv) > 0 || len(f.ContainerEnv) > 0 {
		spec.Env = make(map[string]string, len(f.RemoteEnv)+len(f.ContainerEnv))
		for k, v := range f.RemoteEnv {
			spec.Env[k] = v
		}
		for k, v := range f.ContainerEnv {
			spec.Env[k] = v
		}
	}

	if len(f.Mounts) > 0 {
		spec.Mounts = append([]string(nil), f.Mounts...)
	}

	switch {
	case f.RemoteUser != "":
		spec.User = f.RemoteUser
	case f.ContainerUser != "":
		spec.User = f.ContainerUser
	}

	if !f.PostCreateCommand.Empty() {
		spec.PostCreate = strings.TrimSpace(f.PostCreateCommand.AsShell())
	}

	return spec
}
