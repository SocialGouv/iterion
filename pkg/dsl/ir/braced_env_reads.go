package ir

import "strings"

// BracedEnvRead is one braced env form the engine's expansion consults: the
// C-identifier name it looks up, whether the author wrote a `:-` arm, and
// where the form opens in the body.
type BracedEnvRead struct {
	Name string
	// HasDefault marks the `${NAME:-default}` arm, which the engine resolves
	// unconditionally; a plain `${NAME}` is answered only when the engine's
	// own environment carries the name and its process-env policy exposes it
	// (ProcessEnvReadable).
	HasDefault bool
	// At is the offset of the `$` the form opens at — the position the
	// engine consults this name FROM, which a consumer intersects with the
	// positions a shell reads. A name consulted twice in one body is two
	// reads here, each with its own arm: the engine's answer to a form is
	// the form's, not the name's first occurrence's.
	At int
}

// BracedEnvReads replays the engine's braced-env segmentation over body and
// returns every form its expansion consults, in body order. The segmentation
// is expandBracedEnv's (pkg/backend/model/executor_tool.go): a form opens at
// `${`, its segment ends at the FIRST `}` (strings.IndexByte — a nested
// `${…}` inside is never a segment of its own, and its offset never appears
// here), the name is the text up to the first `:-`, and the form is consulted
// only when that name is a C identifier; an unterminated `${` passes the rest
// of the body through verbatim and ends the walk. The pin test in
// pkg/backend/model holds this reader to the engine's behaviour on the whole
// disagreement class (braced_env_reads_pin_test.go).
func BracedEnvReads(body string) []BracedEnvRead {
	var reads []BracedEnvRead
	for i := 0; i < len(body); {
		if body[i] != '$' || i+1 >= len(body) || body[i+1] != '{' {
			i++
			continue
		}
		end := strings.IndexByte(body[i+2:], '}')
		if end == -1 {
			// Unterminated: the engine hands the rest of the body through
			// verbatim, so no later form is consulted either.
			break
		}
		if name, hasDefault, ok := envRefName(body[i+2 : i+2+end]); ok {
			reads = append(reads, BracedEnvRead{Name: name, HasDefault: hasDefault, At: i})
		}
		i += 2 + end + 1
	}
	return reads
}

// envRefName is the engine's own reader predicate over one braced segment:
// the name is the text up to the first `:-`, and the form is an env ref
// only when that name is a C identifier ([A-Za-z_][A-Za-z0-9_]*) — the rule
// looksLikeEnvRef applies at pkg/backend/model/executor_tool.go, which keeps
// `${X#y}`, `${X:+y}`, `${X%y}`, `${#X}` and script-language template
// literals (`${p.name}`) out of the expansion. The one place the predicate
// lives: the engine's half of the C188 read set is BracedEnvReads', and the
// shell's half records positions only.
func envRefName(seg string) (name string, hasDefault bool, ok bool) {
	if idx := strings.Index(seg, ":-"); idx != -1 {
		name, hasDefault = seg[:idx], true
	} else {
		name = seg
	}
	if !isCIdentifier(name) {
		return "", false, false
	}
	return name, hasDefault, true
}

// bracedEnvGluedToAName reports whether the byte after the braced form at
// `at` continues a bash name ([A-Za-z0-9_]) — the one case where dropping
// the braces changes what the shell reads, because `$NAME` takes the LONGEST
// name it can and swallows that byte into it (`"${COUNT}_items"` says COUNT
// and `_items`, `"$COUNT_items"` says the name COUNT_items). `at` sits on
// the `$` of a form whose closing brace exists.
func bracedEnvGluedToAName(body string, at int) bool {
	end := strings.IndexByte(body[at+2:], '}')
	if end == -1 {
		return false
	}
	next := at + 2 + end + 1
	return next < len(body) && (isAlnum(body[next]) || body[next] == '_')
}

// isCIdentifier reports whether s matches [A-Za-z_][A-Za-z0-9_]*.
func isCIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b == '_':
		case b >= '0' && b <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
