// Package skilllib implements iterion's first-class skill library: a
// standalone, operator-curated store of Claude-Code-style SKILL.md skills,
// stored globally (~/.iterion/skills/) with an optional per-project override,
// and referenced from workflows via the DSL `skills:` field. See ADR-059 and
// docs/skills-library.md.
//
// The library is the hand-authored/editable half of the "hybride" model; the
// plugin path (plugin.SynthesizeSkillsManifest) remains the third-party-pack
// import route. Both mirror into <workspace>/.claude/skills/ at run start via
// the shared runtime.reconcileSkillFile collision policy.
package skilllib

import (
	"bufio"
	"io"
	"strings"
)

// ScanFrontmatter reads leading YAML-ish frontmatter (delimited by `---`
// lines) from r and returns the `name:` and `description:` fields. It is
// tolerant of a missing frontmatter block (returns empty strings) and never
// errors on content — only the caller's file open can fail. This is the single
// shared parser used by both the skill library and runview's bundle-skill
// catalog (runview.readSkillFile), so the two never drift.
//
// Block scalars are supported: `description: >` (folded — continuation lines
// joined into one whitespace-normalized string) and `description: |` (literal
// — continuation lines joined with newlines). This matters because a skill's
// routable text often lives in a multi-line block; without it the router would
// see just the ">"/"|" indicator as the whole description.
func ScanFrontmatter(r io.Reader) (name, description string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	// Walk to the opening "---", then collect key:value lines until the
	// closing "---". Tolerant of leading whitespace and a file with no
	// frontmatter block at all (the first content line ends the scan).
	opened := false

	// Block-scalar collection state. blockKey is non-empty while gathering a
	// `>`/`|` value; fold selects folded (space-join) vs literal (newline-join);
	// baseIndent is the key line's indent — a continuation line must be more
	// indented (blank lines are part of the block).
	var blockKey string
	var fold bool
	var baseIndent int
	var block []string
	assign := func(key, val string) {
		switch key {
		case "name":
			name = val
		case "description":
			description = val
		}
	}
	flushBlock := func() {
		if blockKey == "" {
			return
		}
		var joined string
		if fold {
			joined = strings.Join(strings.Fields(strings.Join(block, " ")), " ")
		} else {
			joined = strings.TrimRight(strings.Join(block, "\n"), "\n")
		}
		assign(blockKey, joined)
		blockKey, block = "", nil
	}

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if !opened {
			if trimmed == "---" {
				opened = true
				continue
			}
			if trimmed == "" {
				continue
			}
			break // first non-frontmatter content line → no frontmatter
		}
		if blockKey != "" {
			indent := len(line) - len(strings.TrimLeft(line, " \t"))
			if trimmed == "" {
				block = append(block, "")
				continue
			}
			// A more-indented line is block content — including one that reads
			// "---" or "key:", which are literal text inside the scalar, not
			// structure. Only a dedent to the key's indent (or less) ends it.
			if indent > baseIndent {
				block = append(block, trimmed)
				continue
			}
			flushBlock() // dedent ends the block; reprocess this line below
		}
		if trimmed == "---" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(k))
		if key != "name" && key != "description" {
			continue
		}
		val := strings.TrimSpace(v)
		if isBlockScalarHeader(val) {
			// A pure block-scalar header (`>`/`|` + optional chomping/indent
			// indicator, nothing else) opens a multi-line value. `key: > text`
			// with content on the same line is NOT a block — it falls through
			// and is assigned as an inline scalar.
			blockKey = key
			fold = val[0] == '>'
			baseIndent = len(k) - len(strings.TrimLeft(k, " \t"))
			block = nil
			continue
		}
		val = strings.TrimPrefix(val, "\"")
		val = strings.TrimSuffix(val, "\"")
		assign(key, val)
	}
	flushBlock() // frontmatter that ends at EOF mid-block
	return name, description
}

// isBlockScalarHeader reports whether a value is a YAML block-scalar header:
// `>` or `|`, optionally followed by a chomping indicator (`+`/`-`) and/or a
// numeric indentation indicator, and nothing else. `> some text` (content on
// the same line) is not a header — it is an inline value.
func isBlockScalarHeader(val string) bool {
	if val == "" || (val[0] != '>' && val[0] != '|') {
		return false
	}
	for _, r := range val[1:] {
		if r != '+' && r != '-' && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
