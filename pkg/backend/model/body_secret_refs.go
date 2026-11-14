package model

import (
	"errors"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
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
// A marker never leaves the executor: shown turns each back into its
// reference's text — the placeholder — for every surface that persists or
// shows the body (hooks, events, the run log, the self-repair prompt), and
// the next rendering draws others. A reference nothing materialises — a file
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

// replace rewrites s in one pass, each marker by what read answers for it —
// the occurrence it reads (the marker, or the marker single-quoted) and its
// replacement. One pass: what a marker resolves to is never read again.
func (b *bodySecrets) replace(s string, read func(m string) (from, to string)) string {
	if b == nil || len(b.order) == 0 {
		return s
	}
	pairs := make([]string, 0, 2*len(b.order))
	for _, m := range b.order {
		from, to := read(m)
		pairs = append(pairs, from, to)
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

// shown is s with each marker as its reference's text: the form hooks,
// events, logs and prompts carry.
func (b *bodySecrets) shown(s string) string {
	return b.replace(s, func(m string) (string, string) { return m, b.text[m] })
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
	out := b.replace(s, func(m string) (string, string) {
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
		return from, to
	})
	return out, env
}

// script resolves the markers of a `script:` body: each marker by the
// guard's reading of its text in a script (MaterializeShell) — the value of
// a secret granting `command`, its text otherwise.
func (b *bodySecrets) script(s string) string {
	return b.replace(s, func(m string) (string, string) { return m, b.guard.MaterializeShell(b.text[m]) })
}

// authoredBody is one run's rendering of an authored tool body: its text,
// each secret reference the author wrote as a marker of secrets, and the
// table that resolves them (nil when no guard stands behind the rendering).
type authoredBody struct {
	text    string
	secrets *bodySecrets
}

// shown is the body as hooks, events, logs and the self-repair prompt carry
// it: each marker as its reference's text, the placeholder.
func (b authoredBody) shown() string { return b.secrets.shown(b.text) }

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

// scriptFor is the script body written for resolved, the recipe's report of
// b: b's markers resolved (bodySecrets.script).
func (b authoredBody) scriptFor(resolved string) (string, error) {
	if resolved != b.shown() {
		return "", errBodyDiverged
	}
	return b.secrets.script(b.text), nil
}
