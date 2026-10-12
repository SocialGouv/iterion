package delegate

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/ambient"
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
	// kimi-code 2.1.1 reads the operator's $KIMI_CODE_HOME/AGENTS.md and
	// ~/.agents/AGENTS.md, then the workspace's AGENTS.md, agents.md and
	// .kimi-code/AGENTS.md from the git root down to the working directory —
	// never above the root (TestCLILiveKimiAmbientPolicies). The workspace
	// policy is served a home without its AGENTS.md; no switch skips the
	// walk, so none and operator are refused (pkg/backend/ambient).
	Ambient: &CLIAgentAmbient{
		HomeEnv:          "KIMI_CODE_HOME",
		DefaultHome:      ".kimi-code",
		InstructionFiles: []string{"AGENTS.md"},
		HostInstructions: kimiHostInstructions,
	},
	// MapEffort nil: kimi-code has no reasoning-effort dial.
	// ResolveEnv nil: kimi reads its own credentials from the host env/config.
}

// kimiGenericInstructionNames are the files kimi reads from ~/.agents, the
// first one found winning.
var kimiGenericInstructionNames = []string{"AGENTS.md", "agents.md"}

// kimiHostInstructions reports the instruction files kimi would read from
// outside the workspace walk for this task: it resolves the user's home from
// HOME, the spawn's own value first, so no overlay of its home can withhold
// those files. When the workspace's repo root IS the home (dotfiles repos, CI
// images whose HOME sits in a repository), kimi's walk starts at the home and
// its root-level files load too — operator files as far as the policy is
// concerned, so they are reported as well.
func kimiHostInstructions(task Task) []string {
	home := taskEnv(task, "HOME")
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return nil
		}
	}
	var files []string
	for _, name := range kimiGenericInstructionNames {
		path := filepath.Join(home, ".agents", name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			files = append(files, path)
			break
		}
	}
	if root := ambient.RepoRoot(task.WorkDir); root != "" && filepath.Clean(root) == filepath.Clean(home) {
		if info, err := os.Stat(filepath.Join(home, ".kimi-code", "AGENTS.md")); err == nil && !info.IsDir() {
			files = append(files, filepath.Join(home, ".kimi-code", "AGENTS.md"))
		}
		for _, name := range kimiGenericInstructionNames {
			path := filepath.Join(home, name)
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				files = append(files, path)
				break
			}
		}
	}
	return files
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
