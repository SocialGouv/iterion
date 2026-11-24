package runtime

// The untrusted-workspace skill channel (iterion#1719).
//
// On a run whose workspace the operator does not control (store.RunTrust
// not default — a checkout under review), the directories every skill
// lookup walks answer with text the CHECKOUT planted: a fork commits
// .agents/skills/review/SKILL.md and the agent is served attacker prompt
// text under the bot's own skill name. The run therefore serves only the
// skills the mirror OWNS (what iterion wrote, excluding what the workspace
// shadowed), re-carried into an engine-owned root:
//
//   - claw's `skill` tool resolves through the pinned name → path
//     allowlist (claw-code-go's ExecuteSkillPinned) — no root is walked;
//   - claude_code discovers no pinned files but directories: the root is
//     handed over as `--add-dir`, the CLI's documented discovery for skills
//     outside the working tree. The floor that keeps the checkout's own
//     .claude/skills out (withholding the project setting source) is the
//     untrusted-lane work of #1730; this channel is what restores the
//     bot's OWN skills once that floor stands.
//
// The root lives INSIDE the workspace (the one tree every sandbox driver
// carries — the kubernetes driver bind-mounts nothing, owned_skills.go
// records why) but OUTSIDE the mirrored .claude/skills: it is removed and
// rewritten on every mirror pass, so the engine is its only writer and a
// name the run does not own does not exist in it. The named residue is
// owned_skills.go's own: a node running a shell EARLIER in the same run
// can rewrite it; what the channel removes is the repository's ability to
// supply the content before the run starts.

import (
	"fmt"
	"path/filepath"
	"strings"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// skillChannelDirName is the directory under <workDir>/.claude/ the run's
// owned skills are re-carried into, in the .claude/skills layout a
// directory-discovering backend expects below an --add-dir root.
const skillChannelDirName = "iterion-run-skills"

// skillChannelRoot returns the engine-owned skill channel root for a
// workspace.
func skillChannelRoot(workDir string) string {
	return filepath.Join(workDir, ".claude", skillChannelDirName)
}

// mirrorSkillChannel re-carries the owned skills into the channel root and
// returns the root plus the pinned name → SKILL.md path allowlist. A
// directory skill serves its <name>/SKILL.md, a flat <stem>.md serves the
// file — the two shapes the workspace mirror writes. The FIRST owner of a
// name wins: owned arrives in mirror precedence order (bundle, plugin,
// library — ADR-059), and a collision the workspace mirror already decided
// is not re-decided here.
//
// The root is reset unconditionally: the checkout may carry a directory of
// the same name, and reading any of it back is the failure the channel
// exists against. Every read and write below goes through the pass's
// workspace handle: a path that escapes the workspace is refused typed,
// never followed.
func mirrorSkillChannel(workDir string, owned []string, pass *mirrorPass, logger *iterlog.Logger) (string, map[string]string, error) {
	root := skillChannelRoot(workDir)
	wr := pass.root()
	if err := wr.RemoveAll(root); err != nil {
		return "", nil, fmt.Errorf("runtime: reset the skill channel root: %w", err)
	}
	dest := filepath.Join(root, ".claude", "skills")
	if err := wr.MkdirAll(dest, 0o755); err != nil {
		return "", nil, fmt.Errorf("runtime: create the skill channel root: %w", err)
	}
	allowlist := make(map[string]string, len(owned))
	for _, ownedPath := range owned {
		name := filepath.Base(ownedPath)
		info, err := wr.Stat(ownedPath)
		if err != nil {
			// The mirror reported the path this same pass; a vanishing
			// entry is the workspace moving under the run — skip it rather
			// than fail the channel, its absence is the honest answer.
			if logger != nil {
				logger.Warn("skill channel: owned skill %q unreadable, left out: %v", ownedPath, err)
			}
			continue
		}
		if info.IsDir() {
			if _, taken := allowlist[name]; taken {
				continue
			}
			dst := filepath.Join(dest, name)
			if err := wr.CopyTreeWithin(ownedPath, dst); err != nil {
				return "", nil, fmt.Errorf("runtime: skill channel: copy %q: %w", name, err)
			}
			allowlist[name] = filepath.Join(dst, "SKILL.md")
			continue
		}
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		if _, taken := allowlist[stem]; taken {
			continue
		}
		dst := filepath.Join(dest, name)
		if err := wr.CopyWithin(ownedPath, dst); err != nil {
			return "", nil, fmt.Errorf("runtime: skill channel: copy %q: %w", name, err)
		}
		allowlist[stem] = dst
	}
	return root, allowlist, nil
}
