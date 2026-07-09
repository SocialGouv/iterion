package delegate

import (
	"strings"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// BackendKimi is the registration name for the Moonshot kimi-code backend
// (`backend: "kimi"` in the DSL). It drives Moonshot's `kimi` CLI, whose
// argument protocol is disjoint from claude-code's (it takes the prompt as
// `-p <prompt>`, not `--print` with the prompt on stdin), so neither
// `backend: "claude_code"` nor a per-node `command:` override (#76) can run
// it. See ADR-065.
const BackendKimi = "kimi"

// kimiProtocol describes Moonshot's kimi-code CLI:
//
//	kimi -p <prompt> --output-format stream-json [-m <alias>]
//
// The prompt (with the node's `system:` task folded in as a preamble, since
// kimi exposes no system-prompt flag) is passed as the value of `-p`; the CLI
// emits a claude-code-style stream-json event stream we parse for the final
// assistant text. Credentials/endpoint are resolved by the CLI itself from its
// own environment/config (e.g. $MOONSHOT_API_KEY), so ResolveEnv is nil.
var kimiProtocol = CLIAgentProtocol{
	Name:             BackendKimi,
	DefaultBinary:    "kimi",
	PromptFlag:       "-p",
	OutputFormatFlag: "--output-format",
	OutputFormat:     "stream-json",
	ModelFlag:        "-m",
	MapModel:         kimiMapModel,
	ParseOutput:      parseStreamJSONText,
	// MapEffort nil: kimi-code has no reasoning-effort dial.
	// ResolveEnv nil: kimi reads its own credentials from the host env/config.
}

// NewKimiBackend constructs the Moonshot kimi-code backend. command overrides
// the default `kimi` binary (a pinned build / wrapper path); empty uses the
// binary on PATH.
func NewKimiBackend(logger *iterlog.Logger, command string) *CLIAgentBackend {
	return &CLIAgentBackend{
		Protocol: kimiProtocol,
		Command:  command,
		Logger:   logger,
	}
}

// kimiMapModel translates an iterion model spec into the alias kimi's `-m`
// flag expects. iterion specs are `provider/model` (e.g. "moonshot/kimi-k2");
// kimi wants the bare alias, so we strip a leading provider segment. A spec
// with no "/" is passed through unchanged (already an alias, e.g. "kimi-k2").
func kimiMapModel(model string) string {
	model = strings.TrimSpace(model)
	if i := strings.Index(model, "/"); i >= 0 {
		return model[i+1:]
	}
	return model
}
