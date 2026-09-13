package model

import "context"

// InvocationFiles is an immutable, invocation-scoped file view. The host
// paths are used by local tools and the runtime's verifier; sandbox paths
// name the same bind-mounted area inside the run container. It is carried
// on the call context so parallel nodes never mutate the shared executor's
// workDir or legacy artifactFilesDir fields.
type InvocationFiles struct {
	HostDir        string
	SandboxDir     string
	HasOutputFiles bool
	Inputs         map[string]InvocationFileInput
}

type InvocationFileInput struct {
	HostPath    string
	SandboxPath string
	SHA256      string
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

// Resolve only verified descriptors supplied by the native coordinator. Keep
// the caller's value tree unchanged because its logical references are used
// for admission fingerprints and checkpoint publication.
func invocationInputPaths(value any, paths map[string]InvocationFileInput, sandboxed bool) any {
	switch v := value.(type) {
	case map[string]any:
		if path, ok := v["path"].(string); ok {
			if mapped, exists := paths[path]; exists && v["sha256"] == mapped.SHA256 {
				if sandboxed {
					return mapped.SandboxPath
				}
				return mapped.HostPath
			}
		}
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = invocationInputPaths(item, paths, sandboxed)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for index, item := range v {
			out[index] = invocationInputPaths(item, paths, sandboxed)
		}
		return out
	default:
		return value
	}
}
