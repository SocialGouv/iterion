package model

import "context"

// InvocationFiles is an immutable, invocation-scoped output area. The host
// path is used by local tools and the runtime's verifier; the sandbox path
// names that SAME bind-mounted area inside the run container. It is carried
// on the call context so parallel nodes never mutate the shared executor's
// workDir or legacy artifactFilesDir fields.
type InvocationFiles struct {
	HostDir    string
	SandboxDir string
}

type invocationFilesContextKey struct{}

func WithInvocationFiles(ctx context.Context, files InvocationFiles) context.Context {
	return context.WithValue(ctx, invocationFilesContextKey{}, files)
}

func InvocationFilesFromContext(ctx context.Context) (InvocationFiles, bool) {
	files, ok := ctx.Value(invocationFilesContextKey{}).(InvocationFiles)
	return files, ok
}

func (e *ClawExecutor) artifactFilesDirForCall(ctx context.Context, sandboxed bool) string {
	if files, ok := InvocationFilesFromContext(ctx); ok {
		if sandboxed {
			return files.SandboxDir
		}
		return files.HostDir
	}
	return e.artifactFilesDir
}
