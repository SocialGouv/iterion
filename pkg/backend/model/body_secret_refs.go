package model

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
)

// bodySecrets is one rendering's table of the secret references the AUTHOR
// wrote in a tool body — a `command:`, a `postcondition:`, a `script:`: the
// only text of a body that ever becomes a secret (ADR-146, amendment of
// 2026-11-14).
//
// It is authoredSecrets's design (action_secret_refs.go) carried to the
// bodies. A placeholder cannot carry provenance: `__ITERION_SECRET_X__` is
// deterministic, so data spells it as well as the author does — a model
// writes it into an output an edge maps, a webhook puts it in a launch var,
// two raw `{{!…}}` values join into it, the author's own text abuts a value
// that completes it. So each authored reference the guard may materialise —
// a `{{secrets.NAME}}`, the placeholder a server-minted `{{vars.x}}` renders
// to — becomes a marker drawn from crypto/rand for this one rendering, which
// no data can spell, and the body's materialiser resolves those markers and
// nothing else: each through the guard, on the reference's own text alone,
// never on the body around it. A placeholder that arrives through data stays
// the text it is, whatever its secret grants.
//
// A marker reaches no hook, event, log or model: shown turns each back into
// its reference's text — the placeholder — for every surface that persists
// or shows the body, and the next rendering draws others. The one place it
// stands is the process the body runs in: a shell script names the
// variable carrying a value after it (script). A recovery model reads the body's repair form instead
// (authoredBody.repair), where every placeholder that is not a marker is
// defused first: shown alone, a data placeholder would look exactly like the
// author's reference, and the model's echo of it would be materialised by
// the agent sink. A reference nothing materialises — a file
// secret's mounted path, an undeclared name, a value under the placeholder
// kill switch — renders its text as before: there is nothing to resolve.
//
// A nil table — no guard behind the rendering — draws nothing.
type bodySecrets struct {
	guard *secretguard.Guard
	// byText is the marker of each reference text, drawn on first use.
	byText map[string]string
	// text is each marker's reference text: what it stands for everywhere
	// but the process the body runs in.
	text map[string]string
	// order is the markers in the order they were drawn.
	order []string
}

func newBodySecrets(guard *secretguard.Guard) *bodySecrets {
	if guard == nil {
		return nil
	}
	return &bodySecrets{guard: guard, byText: map[string]string{}, text: map[string]string{}}
}

// markFunc is resolveTemplateWith's mark for this table: nil when there is
// no table, which renders every reference as its text.
func (b *bodySecrets) markFunc() func(text string) string {
	if b == nil {
		return nil
	}
	return b.mark
}

// mark renders an authored reference whose text is text: the marker standing
// for it in this rendering, drawn on its first reference, when the guard may
// materialise it (a declared value secret's placeholder, an env-only
// credential's); the text itself otherwise.
func (b *bodySecrets) mark(text string) string {
	if !b.guard.NamesMaterializableSecret(text) && !b.guard.NamesEnvOnlySecret(text) {
		return text
	}
	if m, ok := b.byText[text]; ok {
		return m
	}
	m := drawAuthoredMarker()
	b.byText[text] = m
	b.text[m] = text
	b.order = append(b.order, m)
	return m
}

// replace rewrites s in one pass, each marker by what read answers for it:
// old/new pairs, each old an occurrence of the marker (bare, single-quoted,
// in a JSON literal). One pass: what a marker resolves to is never read
// again. Where two olds start at one position the one read answers first
// wins, so a longer form (`"M"`) is answered before the bare marker.
func (b *bodySecrets) replace(s string, read func(m string) []string) string {
	if b == nil || len(b.order) == 0 {
		return s
	}
	pairs := make([]string, 0, 2*len(b.order))
	for _, m := range b.order {
		pairs = append(pairs, read(m)...)
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

// shown is s with each marker as its reference's text: the form hooks,
// events and logs carry.
func (b *bodySecrets) shown(s string) string {
	return b.replace(s, func(m string) []string { return []string{m, b.text[m]} })
}

// shellEnv resolves the markers of a shell body the way the guard resolves an
// authored reference (MaterializeAuthoredShellEnv), each on its own text.
// Single-quoted — how the template layer renders a `{{secrets.NAME}}` and a
// minted `{{vars.x}}` — the quoted marker becomes a reference to the child's
// environment, which carries the value: the value never enters the argv.
// Bare — the author's raw `{{!secrets.NAME}}` — the value is inlined, and an
// env-only credential keeps its text, since it never enters an argv. A
// marker standing both quoted and bare takes the bare reading everywhere, the
// guard's rule for a placeholder written both ways. A reference whose secret
// does not grant `command` keeps its text.
func (b *bodySecrets) shellEnv(s string) (string, map[string]string) {
	var env map[string]string
	out := b.replace(s, func(m string) []string {
		text := b.text[m]
		from, form := m, text
		quoted := "'" + m + "'"
		if n := strings.Count(s, quoted); n > 0 && n == strings.Count(s, m) {
			from, form = quoted, "'"+text+"'"
		}
		to, refEnv := b.guard.MaterializeAuthoredShellEnv(form)
		for name, value := range refEnv {
			if env == nil {
				env = make(map[string]string, len(b.order))
			}
			env[name] = value
		}
		return []string{from, to}
	})
	return out, env
}

// script resolves the markers of a `script:` body written in language, and
// returns the environment the interpreter takes for them. A marker's value
// is the secret's when it grants `command` (MaterializeFor); a reference
// nothing resolves keeps its text, the placeholder, which reads alike in
// every context. Where it resolves, the value is written for the syntax the
// marker stands in:
//
//   - The default form renders as a JSON string literal holding the marker
//     (`"M"`, renderScript's jsonLiteralValue), and so does a raw reference
//     the author double-quoted; `"{{secrets.NAME}}"` renders `""M""`, read
//     as the one literal it is. In a shell script the literal becomes
//     `"$M"` — a variable named after the marker, which no data can spell —
//     and the value rides the interpreter's environment, never the
//     script's text. Nor can data read it: a shell script's data rides the
//     environment too (renderShellScript), its value never in the script's
//     text, so no data runs as code — no `$(env)`, no bash `${!prefix@}`
//     then `${!x}`. Where the shell expands nothing — single quotes, a
//     heredoc whose delimiter is quoted — the script receives the
//     variable's name and not the value: C342 warns on those positions at
//     authoring time. In js and py the literal is the value's JSON string
//     literal with every character that closes or interpolates a string
//     (`'`, the backquote, `$`, `{`, `}`, `/`) written as a `\u` escape
//     (scriptStringLiteral). In the expression position the value reads
//     back byte for byte. Inside quoting of the author's own — single or
//     double quotes, an f-string, a template literal — it can close or
//     interpolate none of it and runs as no code, but the language cooks
//     the literal once: an author who parses that text again (JSON in a
//     string) reads what the value spells, structure included. Only the
//     expression position is exact.
//   - The raw form `{{!secrets.NAME}}`, bare, is the author's own text:
//     escaped for a single-quoted word in a shell script, as in a command
//     (MaterializeShell), and verbatim in js and py, where the author's
//     quoting decides.
//
// A value no form carries as written is refused, naming the reference and
// never the value: a NUL byte in a shell script, which neither an
// environment variable nor the script's text can hold, and invalid UTF-8 in
// a js or py literal, which JSON would silently rewrite.
func (b *bodySecrets) script(s, language string) (string, map[string]string, error) {
	if b == nil || len(b.order) == 0 {
		return s, nil, nil
	}
	shell := scriptRunsInAShell(language)
	var env map[string]string
	pairs := make([]string, 0, 4*len(b.order))
	for _, m := range b.order {
		text := b.text[m]
		literal := `"` + m + `"`
		// The default form inside the author's own double quotes:
		// `"{{secrets.NAME}}"` renders `""M""`, and the whole of it is the
		// one literal the author meant.
		doubled := `""` + m + `""`
		quoted := strings.Contains(s, literal)
		value := b.guard.MaterializeFor(secretsink.Command, text)
		lit, raw := scriptStringLiteral(language, text), text
		switch {
		case value == text:
			// Nothing resolves: the reference's text in either form.
		case shell:
			if strings.ContainsRune(value, 0) {
				return "", nil, fmt.Errorf("the value of the secret %s holds a NUL byte, which a shell script can carry neither in its environment nor in its text", text)
			}
			if quoted {
				if env == nil {
					env = make(map[string]string, len(b.order))
				}
				env[m] = value
			}
			lit, raw = `"$`+m+`"`, b.guard.MaterializeShell(text)
		default:
			if quoted && !utf8.ValidString(value) {
				return "", nil, fmt.Errorf("the value of the secret %s is not valid UTF-8, which a %s string literal cannot carry byte for byte — write the raw {{!secrets.…}} form to splice the bytes as they are", text, language)
			}
			lit, raw = scriptStringLiteral(language, value), value
		}
		pairs = append(pairs, doubled, lit, literal, lit, m, raw)
	}
	return strings.NewReplacer(pairs...).Replace(s), env, nil
}

// scriptRunsInAShell reports whether a `script:` in language is a shell
// program — asked of scriptInterpreter, the one reading of the languages.
func scriptRunsInAShell(language string) bool {
	argv, _ := scriptInterpreter(language)
	return len(argv) > 0 && (argv[0] == "sh" || argv[0] == "bash")
}

// shellDoubleQuoted escapes the four characters a double-quoted shell word
// interprets — `\`, `"`, `$` and the backquote; a newline inside the quotes
// is itself.
var shellDoubleQuoted = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`")

// scriptQuoteSafe writes, in a JSON string literal, each character a js or
// py string context the author opened around it reads as syntax — a quote
// that closes it, the backquote and `${` of a template literal, the braces
// of an f-string, the slash of a regular expression — as a `\u` escape,
// which both languages read back as the character. None of them is part of
// any escape JSON writes, so the rewrite leaves those intact.
var scriptQuoteSafe = strings.NewReplacer(
	"'", `\u0027`, "`", `\u0060`, "$", `\u0024`, "{", `\u007b`, "}", `\u007d`, "/", `\u002f`)

// scriptStringLiteral is v as a string literal of a script in language: a
// double-quoted word for a shell, whose expansions are escaped; for js and
// py, a JSON string literal — JSON's string syntax is a subset of both, an
// escape it writes (`\"`, `\\`, `\n`, `\u003c`) reads the same in both —
// with the characters scriptQuoteSafe names escaped too.
func scriptStringLiteral(language, v string) string {
	if scriptRunsInAShell(language) {
		return `"` + shellDoubleQuoted.Replace(v) + `"`
	}
	return scriptQuoteSafe.Replace(jsonLiteralValue(v, ShapeUndeclared))
}

// repairOutput is a recipe's output as the self-repair prompt quotes it:
// every known value back to its placeholder, as on every surface a model
// reads (Unmaterialize), and every placeholder the output already spelled
// as TEXT — data the command printed, or one a printed fragment completes
// beside a value — defused, since echoed into the corrected command the
// agent sink would resolve it. The values go to markers of this one reading
// first, so the defuse can tell the placeholders it must keep from the
// ones it must not, and a fragment abutting a value cannot borrow a
// placeholder's bytes to spell another's.
func repairOutput(g *secretguard.Guard, out string) string {
	t := newBodySecrets(g)
	if t == nil {
		return out
	}
	return t.shown(g.DefuseAll(g.UnmaterializeEach(out, t.mark)))
}

// authoredBody is one run's rendering of an authored tool body: its text,
// each secret reference the author wrote as a marker of secrets, and the
// table that resolves them (nil when no guard stands behind the rendering).
type authoredBody struct {
	text    string
	secrets *bodySecrets
}

// shown is the body as hooks, events and logs carry it: each marker as its
// reference's text, the placeholder.
func (b authoredBody) shown() string { return b.secrets.shown(b.text) }

// repair is the body as a recovery model reads it: shown, after every
// placeholder that is not one of the author's markers — data's, or one the
// author typed as text — is defused (Guard.DefuseAll). Shown alone, such a
// placeholder would read exactly like the author's reference, and a model
// that echoes the command it was shown would have the agent sink
// materialise it. The author's own references stay live: the repaired
// command still reaches what the recipe reached.
func (b authoredBody) repair() string {
	if b.secrets == nil {
		return b.text
	}
	return b.secrets.shown(b.secrets.guard.DefuseAll(b.text))
}

// errBodyDiverged refuses to run a body other than the one the recipe
// reported: the recipe's resolve hands its shown form to the hooks, and
// buildCmd must run exactly that.
var errBodyDiverged = errors.New("the body to run is not the one the recipe rendered and reported")

// shellFor is the shell body that runs for resolved, the recipe's report of b
// (its shown form): b's markers resolved (bodySecrets.shellEnv), and the
// environment the child takes.
func (b authoredBody) shellFor(resolved string) (string, map[string]string, error) {
	if resolved != b.shown() {
		return "", nil, errBodyDiverged
	}
	out, env := b.secrets.shellEnv(b.text)
	return out, env, nil
}

// scriptFor is the script body in language written for resolved, the
// recipe's report of b — b's markers resolved (bodySecrets.script) — and the
// environment its interpreter takes for them.
func (b authoredBody) scriptFor(resolved, language string) (string, map[string]string, error) {
	if resolved != b.shown() {
		return "", nil, errBodyDiverged
	}
	return b.secrets.script(b.text, language)
}
