// Package dotenv parses dotenv files the way every iterion loader plants
// them: ONE Go reader for the CLI's cwd walk (cmd/iterion applyDotEnv),
// the studio's per-project snapshot (pkg/projectenv) and the desktop's
// env file (cmd/iterion-desktop). Before it existed, the studio lane
// handed the raw bytes to a sourcing bash instead — which EXECUTED the
// file's command substitutions on the host and lost every unquoted value
// carrying spaces, so the plant set differed per lane; the H-10 review
// measured both. The parse is the plant set now, and the only thing a
// loader may still hand to a shell is the `$VAR` expansion the legacy
// instance manager's frame keeps (pkg/projectenv), on values this parser
// read and the loader already refused command substitution on.
//
// Application policy — what is refused, what is marked planted, whether
// existing environment wins — stays with each loader (envtrust's one
// predicate serves all three). Only the reading is shared.
package dotenv

import (
	"bufio"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// Entry is one assignment the file spells, in file order — later entries
// overwrite earlier ones at the loader, like the sourcing shell's
// assignments did.
type Entry struct {
	// Key is the trimmed name, without a leading `export `.
	Key string
	// Value is the right-hand side with surrounding whitespace trimmed
	// and ONE balanced pair of surrounding quotes stripped.
	Value string
	// SingleQuoted records a value the file spelled inside '...': bytes
	// the sourcing shell would have kept LITERAL, so an expansion pass
	// must not touch them (`K='$HOME'` plants `$HOME`, not the operator's
	// home). A double-quoted or bare value expands.
	SingleQuoted bool
}

// Parse reads path and returns its assignments in file order. The format
// is the standard one: `KEY=VALUE` per line, optional `#` comments,
// optional surrounding `"` or `'` quotes around the value, an optional
// `export ` prefix on the key. Lines that are not assignments are SKIPPED,
// not handed to any shell — a bare command in the file used to run when
// the studio lane sourced it. A name no shell could export
// (envtrust.Exportable) is skipped too: the provenance marker cannot carry
// it and Windows would fold it. An unreadable file is an error; the
// callers decide whether that stops them.
func Parse(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		// Allow `export KEY=VALUE` lines (common shell convention).
		key = strings.TrimPrefix(key, "export ")
		key = strings.TrimSpace(key)
		if key == "" || !envtrust.Exportable(key) {
			continue
		}
		val := strings.TrimSpace(line[eq+1:])
		entry := Entry{Key: key, Value: val}
		if len(val) >= 2 {
			if val[0] == '"' && val[len(val)-1] == '"' {
				entry.Value = val[1 : len(val)-1]
			} else if val[0] == '\'' && val[len(val)-1] == '\'' {
				entry.Value = val[1 : len(val)-1]
				entry.SingleQuoted = true
			}
		}
		out = append(out, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
