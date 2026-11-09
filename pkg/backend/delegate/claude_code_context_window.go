package delegate

import (
	"maps"
	"os"
	"strconv"
	"strings"
)

// claudeContextWindowEnv stamps the catalog context window the task carries
// into the CLI harness env, so the claude CLI compacts its conversation
// BEFORE the provider ceiling instead of dying on it with a 400
// (CONTEXT_LENGTH_EXCEEDED — a review node loaded ~254k tokens against
// deepseek's 256k window and was refused for one token over). Applied at
// every spawn boundary, next to claudeModelDefaultEnv: host and container,
// work and format passes — one backend serves both claw and claude_code.
//
// Guards, in order:
//   - a window <= 0 means no table knows the model: stamp nothing and the
//     harness keeps its own guess;
//   - an explicit setting wins — in the spawn env or ambient, including an
//     empty value, which suppresses the stamp (the same precedence
//     claudeModelDefaultEnv documents);
//   - a native claude id (or none at all — the CLI's default is one) is
//     skipped: the CLI carries its own windows, including the 1M beta, and
//     the aggregator's number could only shrink them.
func claudeContextWindowEnv(env map[string]string, task Task) map[string]string {
	const key = "CLAUDE_CODE_MAX_CONTEXT_TOKENS"
	if task.ContextWindow <= 0 {
		return env
	}
	if _, ok := env[key]; ok {
		return env
	}
	if _, ok := os.LookupEnv(key); ok {
		return env
	}
	if claudeNativeModelID(task.Model) {
		return env
	}
	out := maps.Clone(env)
	if out == nil {
		out = make(map[string]string, 1)
	}
	out[key] = strconv.Itoa(task.ContextWindow)
	return out
}

// claudeNativeModelID reports whether the model spec names a claude model
// the CLI knows natively. The provider prefix an anthropic-wire gateway
// carries is stripped first: "zai/glm-5.3" rides the claude wire but is no
// claude model; "anthropic/claude-opus-5" is. An empty spec means the CLI
// falls back to its own default — a native claude model.
func claudeNativeModelID(spec string) bool {
	id := claudeCodeModelID(spec)
	return id == "" || strings.HasPrefix(id, "claude-")
}
