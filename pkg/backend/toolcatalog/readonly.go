package toolcatalog

import (
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
)

// ReadonlyWithholdsDurableWrite reports whether a node's `readonly: true`
// removes one durable-write concept from a run on this backend. It is one of
// the two parameters of the shared per-backend question iterion#2882 names —
// "given this backend, this node, this readonly flag and this tools list,
// does a durable write survive?" — the other being DeclaredListBounds, and
// the composition being bundlelint's durable-memory capability predicate,
// which the lint (C274) and its corpus twin both call.
//
// The flag is per-backend leverage, not a uniform contract — the same
// reading each delegate documents at its own enforcement point:
//
//   - claude_code withholds the file-writing natives — the WritingCanonicals
//     projection delegate.claudeReadonlyWithheld applies to every spawn —
//     and KEEPS the shell: the flag is not a sandbox there, so a durable
//     write survives through `bash` and through nothing else
//     (delegate.claudeReadonlyResidualWritePaths names that residue);
//   - codex answers the flag with a read-only OS sandbox
//     (delegate.codexSandboxForTask): no durable write survives, the shell
//     included;
//   - pi pins the tool list to four readers (`--tools read,grep,find,ls`):
//     no durable write survives;
//   - claw, kimi, grok and opencode IGNORE the flag — no lever of theirs
//     reads it — so a durable write survives exactly as without it. That is
//     the measured behaviour, recorded here rather than guessed: a backend
//     that learns a lever updates its row, not every reader of the question.
//
// canonical is a member of DurableWriteCanonicals — fold the candidate
// through CanonicalToolName before asking.
func ReadonlyWithholdsDurableWrite(backend, canonical string) bool {
	switch strings.TrimSpace(backend) {
	case backendcaps.Codex, backendcaps.Pi:
		return true
	case backendcaps.ClaudeCode:
		return slices.Contains(writingCanonicals, canonical)
	default:
		return false
	}
}
