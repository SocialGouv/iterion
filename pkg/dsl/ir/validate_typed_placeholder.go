package ir

import (
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// C340 — a secret's placeholder typed as text in a tool body
// ---------------------------------------------------------------------------
//
// An authored body resolves only the references its author writes —
// `{{secrets.NAME}}`, `{{!secrets.NAME}}`, a minted credential's
// `{{vars.x}}` — each rendered to a marker of one rendering; a placeholder
// spelled as text resolves nowhere, whoever spelled it (ADR-146, amendment
// of 2026-11-14). Before that decision the body's materialiser scanned the
// rendered text and resolved the author's typed placeholder as well, so a
// body that typed one compiled clean and now sends the placeholder itself:
// silently inert. C340 names it at authoring time.
//
// The shapes that are NOT a typed reference stay silent:
//   - an environment read in a shell body (`"$__ITERION_SECRET_X__"`,
//     `${__ITERION_SECRET_X__}`): the placeholder doubles as the variable the
//     author's own reference hands the child, so reading it is a variable
//     read, not a reference;
//   - a server-minted credential's placeholder in a `script:` body: there it
//     IS the documented door — the body names the variable and the engine
//     hands the value to the interpreter of a body that names it (the
//     dep-update-guard shape, `os.environ.get("__ITERION_SECRET_…__")`).
//
// A declared secret's placeholder in a `script:` body is reported whatever
// surrounds it: a declared value never reaches a script's environment, so
// neither reading resolves anything there.
//
// A warning: the bytes run as written, and the author may mean them.
func (c *compiler) validateTypedPlaceholders(w *Workflow) {
	names := make([]string, 0, len(w.Secrets)+len(store.ServerMintedSecretVars))
	minted := map[string]bool{}
	for name := range w.Secrets {
		names = append(names, name)
	}
	for _, name := range store.ServerMintedSecretVars {
		if _, declared := w.Secrets[name]; !declared {
			names = append(names, name)
		}
		minted[name] = true
	}
	if len(names) == 0 {
		return
	}
	slices.Sort(names)
	ids := make([]string, 0, len(w.Nodes))
	for id := range w.Nodes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		t, ok := w.Nodes[id].(*ToolNode)
		if !ok {
			continue
		}
		for _, b := range []struct {
			where  string
			text   string
			script bool
		}{
			{"command", t.Command, false},
			{"postcondition", t.Postcondition, false},
			{"script", t.Script, true},
		} {
			if b.text == "" {
				continue
			}
			text := outsideTemplateRefs(b.text)
			for _, name := range names {
				if b.script && minted[name] {
					continue
				}
				ph := secretsink.PlaceholderFor(name)
				at := typedPlaceholderAt(text, ph, !b.script)
				if at < 0 {
					continue
				}
				ref := "{{secrets." + name + "}}"
				if minted[name] {
					ref = "{{vars." + name + "}}"
				}
				c.warnfAtProperty(DiagTypedSecretPlaceholder, "tool", t.ID, b.where,
					"tool %q %s, body line %d: %s, the placeholder of %q, is typed as text — a body resolves only the references its author writes, so these bytes reach the process as written and resolve to nothing. "+
						"Write %s to hand the process the value",
					t.ID, b.where, strings.Count(text[:at], "\n")+1, ph, name, ref)
			}
		}
	}
}

// outsideTemplateRefs is body with every `{{…}}` span blanked byte for byte,
// newlines kept: what the author typed around the references, at the same
// offsets.
func outsideTemplateRefs(body string) string {
	var b strings.Builder
	b.Grow(len(body))
	for {
		i := strings.Index(body, "{{")
		if i < 0 {
			b.WriteString(body)
			return b.String()
		}
		j := strings.Index(body[i:], "}}")
		if j < 0 {
			b.WriteString(body)
			return b.String()
		}
		b.WriteString(body[:i])
		for k := i; k < i+j+2; k++ {
			if body[k] == '\n' {
				b.WriteByte('\n')
			} else {
				b.WriteByte(' ')
			}
		}
		body = body[i+j+2:]
	}
}

// typedPlaceholderAt is the offset of the first occurrence of ph in text
// that is a typed placeholder, -1 when there is none. In a shell body an
// occurrence read as a variable — `$PH`, `${PH` — is not one.
func typedPlaceholderAt(text, ph string, shell bool) int {
	from := 0
	for {
		i := strings.Index(text[from:], ph)
		if i < 0 {
			return -1
		}
		at := from + i
		before := text[:at]
		if !shell || (!strings.HasSuffix(before, "$") && !strings.HasSuffix(before, "${")) {
			return at
		}
		from = at + len(ph)
	}
}
