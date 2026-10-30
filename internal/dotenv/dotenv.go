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
// optional surrounding `"` or `'` quotes around the value — a comment
// after a closed quote goes, a `#` inside an unquoted value stays — and an
// optional `export ` prefix on the key. Lines that are not assignments are
// SKIPPED, not handed to any shell — a bare command in the file used to run
// when the studio lane sourced it. A name no shell could export
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
			// A quoted value: the closing quote may be followed by a
			// comment — `K="v1" # comment` is the dotenv convention, and
			// the sourcing shell read it the same way. The comment goes;
			// the value is what the quotes held. What the quotes did not
			// hold stays the value it always was: a line whose LAST byte is
			// the quote keeps the outer strip the parser has always done
			// (`K='it''s'` reads `it''s`), and anything else keeps the
			// line-based contract — the bytes are the value, same as the
			// CLI lane plants them. A `#` INSIDE an unquoted value is kept
			// (PASS=abc #123 is somebody's value, not a comment): the strip
			// is paid for by the quotes that close before it.
			quote := val[0]
			if quote == '"' || quote == '\'' {
				inside, rest, ok := cutQuoted(val, quote)
				commentAfter := ok && func() bool {
					rest = strings.TrimSpace(rest)
					return rest == "" || rest[0] == '#'
				}()
				if ok && commentAfter {
					entry.Value = inside
					entry.SingleQuoted = quote == '\''
				} else if val[len(val)-1] == quote {
					// What the quotes did not close on the line keeps the
					// outer strip the parser has always done (`K='it''s'`
					// reads `it''s`); see the branch comment above.
					entry.Value = val[1 : len(val)-1]
					entry.SingleQuoted = quote == '\''
				}
			}
		}
		out = append(out, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// cutQuoted reports the bytes inside the quote that opens val's first byte,
// and what follows the closing quote. Inside double quotes a backslash
// escapes the byte it precedes (the sourcing shell's own rule, so `K="a\"b"`
// keeps the inner quote in the value); single quotes hold no escapes — the
// next quote closes. ok is false when the first byte is not the quote or no
// closing quote exists on the line, in which case neither result means
// anything.
func cutQuoted(val string, quote byte) (inside, rest string, ok bool) {
	if len(val) < 2 || val[0] != quote {
		return "", "", false
	}
	for i := 1; i < len(val); i++ {
		if quote == '"' && val[i] == '\\' && i+1 < len(val) {
			i++ // the escaped byte rides along, quote or not
			continue
		}
		if val[i] == quote {
			return val[1:i], val[i+1:], true
		}
	}
	return "", "", false
}
