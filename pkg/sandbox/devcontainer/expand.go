package devcontainer

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// expandLocalVars resolves the host-side devcontainer.json variable
// substitutions per https://containers.dev/implementors/json_reference/.
// We expand ONLY the variables whose values are knowable on the host
// before docker run (`${localEnv:NAME}`, `${localEnv:NAME:default}`,
// `${localWorkspaceFolder}`, `${localWorkspaceFolderBasename}`).
// Container-side variables (`${containerEnv:...}`,
// `${containerWorkspaceFolder*}`) are left untouched — those are
// resolved at runtime by lifecycle commands inside the container.
//
// The runtime invokes this on every host-bound string before passing
// it to docker (mounts, image, runArgs, postCreate, ...). Without it,
// any devcontainer.json that mounts `${localEnv:HOME}/.claude` (which
// is the canonical Claude Code dev pattern) blows up at `docker run`
// with "invalid mount path: '${localEnv:HOME}/...' must be absolute".
func expandLocalVars(s, repoRoot string) string {
	if s == "" {
		return s
	}
	// ${localWorkspaceFolder*} are positional, no inner colon.
	if repoRoot != "" {
		s = strings.ReplaceAll(s, "${localWorkspaceFolder}", repoRoot)
		s = strings.ReplaceAll(s, "${localWorkspaceFolderBasename}", filepath.Base(repoRoot))
	}
	return localEnvRegex.ReplaceAllStringFunc(s, func(match string) string {
		// match looks like `${localEnv:NAME}` or `${localEnv:NAME:default}`.
		inner := strings.TrimSuffix(strings.TrimPrefix(match, "${localEnv:"), "}")
		name := inner
		def := ""
		if i := strings.Index(inner, ":"); i >= 0 {
			name, def = inner[:i], inner[i+1:]
		}
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return def
	})
}

// Variable names per the spec are word characters (letters, digits,
// underscore). Default value (after the second colon) accepts anything
// up to the closing brace, so it can include slashes, dots, etc.
var localEnvRegex = regexp.MustCompile(`\$\{localEnv:[A-Za-z_][A-Za-z0-9_]*(?::[^}]*)?\}`)

// ExpandLocalVarsInFile resolves all host-side devcontainer.json
// variables in fields the host needs to read before `docker run`:
// Mounts, RunArgs, ContainerEnv values, RemoteEnv values, Image,
// WorkspaceFolder, WorkspaceMount, and the PostCreateCommand /
// PostStartCommand snippets.
//
// Container-only fields (`containerEnv:` keys, container-side
// commands referencing `${containerEnv:...}`) are left as-is — the
// shell inside the container resolves them with its own environment.
//
// Mutates f in place. Safe to call repeatedly.
func ExpandLocalVarsInFile(f *File, repoRoot string) {
	if f == nil {
		return
	}
	for i, m := range f.Mounts {
		f.Mounts[i] = expandLocalVars(m, repoRoot)
	}
	for i, a := range f.RunArgs {
		f.RunArgs[i] = expandLocalVars(a, repoRoot)
	}
	for k, v := range f.ContainerEnv {
		f.ContainerEnv[k] = expandLocalVars(v, repoRoot)
	}
	for k, v := range f.RemoteEnv {
		f.RemoteEnv[k] = expandLocalVars(v, repoRoot)
	}
	f.Image = expandLocalVars(f.Image, repoRoot)
	f.WorkspaceFolder = expandLocalVars(f.WorkspaceFolder, repoRoot)
	f.WorkspaceMount = expandLocalVars(f.WorkspaceMount, repoRoot)
	if f.PostCreateCommand.Shell != "" {
		f.PostCreateCommand.Shell = expandLocalVars(f.PostCreateCommand.Shell, repoRoot)
	}
	for i, a := range f.PostCreateCommand.Argv {
		f.PostCreateCommand.Argv[i] = expandLocalVars(a, repoRoot)
	}
	if f.PostStartCommand.Shell != "" {
		f.PostStartCommand.Shell = expandLocalVars(f.PostStartCommand.Shell, repoRoot)
	}
	for i, a := range f.PostStartCommand.Argv {
		f.PostStartCommand.Argv[i] = expandLocalVars(a, repoRoot)
	}
	if f.Build != nil {
		f.Build.Dockerfile = expandLocalVars(f.Build.Dockerfile, repoRoot)
		f.Build.Context = expandLocalVars(f.Build.Context, repoRoot)
		for k, v := range f.Build.Args {
			f.Build.Args[k] = expandLocalVars(v, repoRoot)
		}
	}
}
