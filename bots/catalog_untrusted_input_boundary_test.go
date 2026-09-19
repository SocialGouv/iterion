package bots

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestCatalogUntrustedInputBoundaryOnWriteAgents enforces the class contract
// #1324 renders on this bot family: every agent that reads
// repository-derived material AND holds a writing tool or a board/forge
// capability carries an `UNTRUSTED INPUT BOUNDARY` paragraph in its system
// prompt. Without it a scanner rationale, a snippet, a matcher's
// description or a forge-fetched ticket body can push an authoritative-
// looking directive into the LLM, which then files a board issue,
// commits a patch or shell-executes a payload.
//
// The predicate is not spelling-based: we look at (a) the presence of a
// writing tool or a board/forge capability on the AGENT declaration,
// which is stable, and (b) the presence of the MARKER phrase in the
// SYSTEM prompt body — which is a posture cue, not a directive list.
// See `bots/sec-audit-source/skills/sec-audit-source.md` for the
// class contract.
//
// The test carries an ALLOWLIST of class members that are known-missing
// today, with a follow-up-ticket reference each. It is expected to be
// drained over time as those bots grow the paragraph; NEVER used to
// hide a NEW class member's absence — the test errors on any addition
// to the class that is not in the allowlist.
func TestCatalogUntrustedInputBoundaryOnWriteAgents(t *testing.T) {
	// Bots left to other sessions per the fleet contract; do NOT even
	// enumerate their class members. Their maintainers will land the
	// paragraph in their own PRs.
	skipBots := map[string]string{
		"docs-refresh":   "DSL v2 session owns bots/docs-refresh — fleet-contract § 1",
		"adr-cartograph": "DSL v2 session owns bots/adr-cartograph — fleet-contract § 1",
		"campaign":       "modernization campaign owns bots/campaign — fleet-contract § 1",
		"modernize":      "modernization campaign owns bots/modernize — fleet-contract § 1",
	}
	// Class members that are known-missing the marker phrase today. Each
	// gets a follow-up ticket; the entry documents the reason (bot's
	// scope, why the paragraph is a small change but not this PR's).
	// Adding a new entry here MUST be paired with a filed ticket.
	//
	// Follow-up ticket: #1494 (aggregate — one paragraph per bot).
	knownMissing := map[string]string{
		"adr-rechallenge":    "#1494 — three agents (survey_code, file_change_ticket, write_addendum) file tickets or write files off repo-derived analysis",
		"app-dev":            "#1494 — interviewer + finalize_mr write; campaign files board issues",
		"bmady":              "#1494 — analyst/pm/architect/dev all use bash on repo-derived material; dev writes",
		"branch-improve-loop": "#1494 — finalize_mr writes off repo-derived diff",
		"devbox-setup":       "#1494 — generate_devbox writes off detected stack",
		"evolve":             "#1494 — 7 agents file backlog issues off repo-derived investigation",
		"feature-gap-fill":   "#1494 — campaign files board issues",
		"instrument":         "#1494 — campaign + finalize_mr",
		"issue-triage":       "#1494 — triage files board decisions off scanner outputs",
		"product-docs":       "#1494 — campaign + finalize_mr + publish",
		"rgaa-audit":         "#1494 — report_card files board issues, holds write/bash",
		"secured-renovacy":   "#1494 — 11 agents run bash on repo material; several write",
		"ultra11y":           "#1494 — adjudicate + publish",
		"whole-improve-loop": "#1494 — finalize_mr",
		"wiki-gen":           "#1494 — author files board issues",
	}
	// Bots that MUST carry the marker on every class member. This PR ships
	// with sec-audit-* and supply-shield-* in this list — they are the
	// security cluster where the paragraph is already the standard.
	enforced := map[string]bool{
		"sec-audit-source": true,
		"sec-audit-deps":   true,
		"supply-shield":    true,
		"supply-shield-cve": true,
	}

	botsRoot := "."
	// The tests run from bots/, so bot paths are directory names.
	entries, err := os.ReadDir(botsRoot)
	if err != nil {
		t.Fatalf("read bots dir: %v", err)
	}
	failed := 0
	newMissing := []string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		bot := e.Name()
		if _, skip := skipBots[bot]; skip {
			continue
		}
		classMembers, systemPrompts := enumerateClassMembersAndPrompts(t, filepath.Join(botsRoot, bot))
		if len(classMembers) == 0 {
			continue
		}
		enforce := enforced[bot]
		for _, m := range classMembers {
			body := systemPrompts[m.systemPrompt]
			hasMarker := strings.Contains(body, "UNTRUSTED INPUT BOUNDARY")
			if hasMarker {
				continue
			}
			if enforce {
				t.Errorf("bots/%s/main.bot: agent %q (system prompt %q) is a class member (%s) but the system prompt has no UNTRUSTED INPUT BOUNDARY marker. Add the paragraph -- see bots/sec-audit-source/skills/sec-audit-source.md for the class contract.",
					bot, m.agentName, m.systemPrompt, m.reason)
				failed++
				continue
			}
			// Non-enforced bot: only allowed if it is in knownMissing.
			if _, ok := knownMissing[bot]; !ok {
				newMissing = append(newMissing, bot+"/"+m.agentName)
			}
		}
	}
	if failed > 0 {
		t.Logf("%d class members missing the marker on enforced bots", failed)
	}
	if len(newMissing) > 0 {
		sort.Strings(newMissing)
		t.Errorf("new class members without the marker AND not in the knownMissing allowlist: %v. Either add the UNTRUSTED INPUT BOUNDARY paragraph to their system prompt, or extend knownMissing with a follow-up ticket reference (per bots/sec-audit-source/skills/sec-audit-source.md).", newMissing)
	}
}

type classMember struct {
	agentName    string
	systemPrompt string
	reason       string
}

var agentHeader = regexp.MustCompile(`(?m)^(?:agent|judge) (\w+):\s*$`)
var promptHeader = regexp.MustCompile(`(?m)^prompt (\w+):\s*$`)
var topLevelHeader = regexp.MustCompile(`(?m)^(agent|tool|compute|prompt|schema|workflow|edge)\b`)
var capsField = regexp.MustCompile(`(?m)^\s*capabilities:\s*\[([^\]]*)\]`)
var toolsField = regexp.MustCompile(`(?m)^\s*tools:\s*\[([^\]]*)\]`)
var systemField = regexp.MustCompile(`(?m)^\s*system:\s*(\w+)`)

func enumerateClassMembersAndPrompts(t *testing.T, botDir string) ([]classMember, map[string]string) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(botDir, "main.bot"))
	if err != nil {
		return nil, nil
	}
	body := string(src)
	prompts := extractTopLevelBlocks(body, promptHeader)
	agents := extractTopLevelBlocks(body, agentHeader)

	var members []classMember
	for name, ab := range agents {
		caps := ""
		if m := capsField.FindStringSubmatch(ab); m != nil {
			caps = m[1]
		}
		tools := ""
		if m := toolsField.FindStringSubmatch(ab); m != nil {
			tools = m[1]
		}
		sys := ""
		if m := systemField.FindStringSubmatch(ab); m != nil {
			sys = m[1]
		}
		reasons := []string{}
		// Board / forge capabilities that CAUSE writing to a durable
		// store: an issue, a comment, a transition. Reading a board
		// (board.read) alone does NOT put the agent in the class.
		if strings.Contains(caps, "board.create") {
			reasons = append(reasons, "board.create")
		}
		if strings.Contains(caps, "board.label") {
			reasons = append(reasons, "board.label")
		}
		if strings.Contains(caps, "board.transition") {
			reasons = append(reasons, "board.transition")
		}
		if strings.Contains(caps, "forge.") {
			reasons = append(reasons, "forge.*")
		}
		// File-writing tools.
		if strings.Contains(tools, "write_file") {
			reasons = append(reasons, "write_file")
		}
		if strings.Contains(tools, "file_edit") {
			reasons = append(reasons, "file_edit")
		}
		if len(reasons) == 0 {
			continue
		}
		members = append(members, classMember{
			agentName:    name,
			systemPrompt: sys,
			reason:       strings.Join(reasons, ", "),
		})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].agentName < members[j].agentName })
	return members, prompts
}

// extractTopLevelBlocks scans src for occurrences of the given header regex
// and returns a map from the captured name to the body up to the next
// top-level node header.
func extractTopLevelBlocks(src string, header *regexp.Regexp) map[string]string {
	out := map[string]string{}
	matches := header.FindAllStringSubmatchIndex(src, -1)
	for i, m := range matches {
		name := src[m[2]:m[3]]
		start := m[1]
		end := len(src)
		// Find the next top-level header after this one.
		nextIdx := topLevelHeader.FindStringIndex(src[start:])
		if nextIdx != nil {
			end = start + nextIdx[0]
		}
		// If we found another match of the same kind earlier, use its
		// start instead.
		if i+1 < len(matches) && matches[i+1][0] < end {
			end = matches[i+1][0]
		}
		out[name] = src[start:end]
	}
	return out
}
