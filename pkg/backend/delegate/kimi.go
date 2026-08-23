package delegate

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
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
	PermissionHook: &CLIAgentPermissionHook{
		HomeEnv:           "KIMI_CODE_HOME",
		DefaultHome:       ".kimi-code",
		ExcludedEntries:   []string{"config.toml"},
		WriteRegistration: writeKimiPermissionHook,
	},
	// MapEffort nil: kimi-code has no reasoning-effort dial.
	// ResolveEnv nil: kimi reads its own credentials from the host env/config.
}

func writeKimiPermissionHook(realHome, shadowHome, command string) error {
	configPath := filepath.Join(realHome, "config.toml")
	raw, err := os.ReadFile(configPath) // #nosec G304 -- resolved operator CLI home.
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		raw = append(raw, '\n')
	}
	raw = append(raw, []byte("\n[[hooks]]\nevent = \"PreToolUse\"\ncommand = "+strconv.Quote(command)+"\n")...)
	return os.WriteFile(filepath.Join(shadowHome, "config.toml"), raw, 0o600)
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

// kimiMapModel preserves the complete kimi-code model alias. Current aliases
// commonly contain a slash themselves (for example
// "kimi-code/kimi-for-coding"), so treating the prefix as an iterion provider
// and stripping it produces an alias that kimi-code cannot resolve.
func kimiMapModel(model string) string {
	return strings.TrimSpace(model)
}
