package model

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// SecretRefResolver resolves `{{secrets.<name>}}` to what a node may see —
// the opaque placeholder, or a file secret's mounted path. The per-run
// secret guard implements it.
type SecretRefResolver interface {
	ResolveSecretRef(name string) string
}

// TemplateResolver renders the `{{…}}` references of a prompt body the way
// every node renders them — one implementation for the executor and for a
// dry run. Supported namespaces:
//
//   - input.<field>                                  — current node's input
//   - vars.<name>                                    — workflow variables
//   - secrets.<name>                                 — placeholder or mounted path (Secrets)
//   - outputs.<node_id>[.<field>...]                 — upstream node output
//   - loop.<name>.iteration                          — current iteration counter
//   - loop.<name>.max                                — declared loop bound
//   - loop.<name>.previous_output[.<field>...]       — snapshot one iteration behind
//     (a loop the workflow does not declare is not found: no 0 for a misspelling)
//   - artifacts.<publish_name>[.<field>...]          — published artifact
//   - run.<member>                                   — the run namespace (`run.id` always)
//   - attachments.<name>[.path|.url|.mime|.size|.sha256]
//     (.url is not found when no signer is wired or the signer fails — said to Warn)
//
// Cross-namespace references need a TemplateData; with nil they resolve as
// not found. A reference that resolves to nothing is kept as written, and
// reported to Unresolved when a caller listens: the executor is silent (the
// model reads the placeholder), a dry run names it.
type TemplateResolver struct {
	Vars    map[string]any
	Secrets SecretRefResolver
	// Unresolved receives every reference kept as written; nil listens to none.
	Unresolved func(ref string)
	// Warn receives the resolver's warnings — the expansion limit, a
	// presign that fails; nil is silent.
	Warn func(format string, args ...any)
}

// Resolve substitutes the `{{…}}` references of body.
func (r *TemplateResolver) Resolve(body string, input map[string]any, td *TemplateData) string {
	var b strings.Builder
	remaining := body

	for {
		start := strings.Index(remaining, "{{")
		if start == -1 {
			b.WriteString(remaining)
			break
		}
		end := strings.Index(remaining[start:], "}}")
		if end == -1 {
			b.WriteString(remaining)
			break
		}
		end += start + 2

		b.WriteString(remaining[:start])

		ref := strings.TrimSpace(remaining[start+2 : end-2])
		val, resolved := r.ResolveRef(ref, input, td)
		if resolved {
			b.WriteString(val)
		} else {
			// Keep unresolved refs as-is.
			if r.Unresolved != nil {
				r.Unresolved(ref)
			}
			b.WriteString(remaining[start:end])
		}

		remaining = remaining[end:]

		// Guard against excessive expansion from large input values.
		// Truncate at the limit rather than appending the remaining template.
		if b.Len() > maxTemplateExpansionSize {
			if r.Warn != nil {
				r.Warn("template expansion exceeded %d bytes, truncating", maxTemplateExpansionSize)
			}
			break
		}
	}

	return b.String()
}

// ResolveValue resolves a single reference (`vars.x`, `outputs.node.field`,
// `loop.name.iteration`, `attachments.name.size`, …) to its VALUE — the
// map, the number, the text the namespace holds — and true, or (nil, false)
// when the reference resolves to nothing. It is the one lookup behind the
// prompt path (ResolveRef, which formats it) and the tool command / script
// / postcondition path (resolveTemplateWith, which shell-escapes or
// JSON-encodes it), so a counter reaches a script as a number and an
// artifact as an object, never as the text a prompt would show.
func (r *TemplateResolver) ResolveValue(ref string, input map[string]any, td *TemplateData) (any, bool) {
	if ref == ir.LiteralOpenExpression {
		return "{{", true
	}
	parts := strings.SplitN(ref, ".", 2)
	if len(parts) < 2 {
		return "", false
	}

	namespace := parts[0]
	key := parts[1]

	switch namespace {
	case "input":
		// `input.X` accepts dotted sub-paths so prompts can drill into
		// structured fields populated by edge `with`-mappings.
		segs := strings.Split(key, ".")
		v, ok := drillTemplatePath(input, segs)
		if ok {
			return v, true
		}
	case "vars":
		// Drilled to the leaf, like input and outputs: `{{vars.cfg.on}}`
		// is the member `on` of the `json` var cfg, not a var whose name
		// contains a dot. A whole-key lookup answered nothing here while
		// an expression and a tool body both answered the member.
		if v, ok := drillTemplatePath(r.Vars, strings.Split(key, ".")); ok {
			return v, true
		}
	case "secrets":
		// {{secrets.X}} renders the opaque placeholder (Layer 1); the
		// real value is materialised by the secret guard at tool/shell
		// execution. File secrets render their mounted path. With the
		// placeholders kill-switch off value secrets render the real value
		// directly.
		if r.Secrets != nil {
			name := key
			if dot := strings.IndexByte(key, '.'); dot >= 0 {
				name = key[:dot]
			}
			if v := r.Secrets.ResolveSecretRef(name); v != "" {
				return v, true
			}
		}
	case "outputs":
		v, ok := outputsTemplateValue(td, strings.Split(key, "."))
		if !ok {
			return "", false
		}
		return v, true
	case "loop":
		if td == nil {
			return "", false
		}
		segs := strings.Split(key, ".")
		if len(segs) < 2 {
			return "", false
		}
		loopName, field := segs[0], segs[1]
		// A loop no edge declares has no counters: not found, so the
		// reference stays as written and a dry run names it — never a 0
		// that reads as "never". A declared loop is in the bounds map
		// (every loop of the workflow is), or in the counters once crossed.
		if _, declared := td.LoopMaxIterations[loopName]; !declared {
			if _, crossed := td.LoopCounters[loopName]; !crossed {
				return nil, false
			}
		}
		switch field {
		case "iteration":
			return int64(td.LoopCounters[loopName]), true
		case "max":
			return int64(td.LoopMaxIterations[loopName]), true
		case "previous_output":
			prev := td.LoopPreviousOutput[loopName]
			// Render empty string on the first iteration (prev is nil)
			// so prompts that say "vide si premiere iteration" read
			// naturally instead of leaving a literal placeholder.
			if len(segs) == 2 {
				if prev == nil {
					return "", true
				}
				return prev, true
			}
			if prev == nil {
				return "", true
			}
			v, ok := drillTemplatePath(prev, segs[2:])
			if !ok {
				return "", true
			}
			return v, true
		}
	case "artifacts":
		if td == nil {
			return "", false
		}
		segs := strings.Split(key, ".")
		art, ok := td.Artifacts[segs[0]]
		if !ok || art == nil {
			return "", false
		}
		if len(segs) == 1 {
			return art, true
		}
		v, ok := drillTemplatePath(art, segs[1:])
		if !ok {
			return "", false
		}
		return v, true
	case "run":
		return runNamespaceValue("", td, key)
	case "attachments":
		if td == nil {
			return "", false
		}
		segs := strings.Split(key, ".")
		info, ok := td.Attachments[segs[0]]
		if !ok {
			return "", false
		}
		// Default sub-field is the path so {{attachments.X}} reads as
		// the local file path the agent / tool can open.
		sub := "path"
		if len(segs) >= 2 {
			sub = segs[1]
		}
		switch sub {
		case "path":
			return info.Path, true
		case "url":
			// A URL that cannot be signed — no signer wired, or the signer
			// failing — resolves as not found: the placeholder stays, a
			// dry run names it, and the failure is said to Warn. An empty
			// string would read as a value.
			url, err := info.URL()
			if err != nil {
				if r.Warn != nil {
					r.Warn("attachment %s: presigned url not resolved: %v", key, err)
				}
				return "", false
			}
			if url == "" {
				return "", false
			}
			return url, true
		case "mime":
			return info.MIME, true
		case "size":
			return info.Size, true
		case "sha256":
			return info.SHA256, true
		}
	}

	return "", false
}

// ResolveRef resolves a single reference to the text a prompt shows for it
// — ResolveValue formatted: a string as is, anything else as JSON — or
// ("", false) when the reference resolves to nothing.
func (r *TemplateResolver) ResolveRef(ref string, input map[string]any, td *TemplateData) (string, bool) {
	v, ok := r.ResolveValue(ref, input, td)
	if !ok {
		return "", false
	}
	return formatValue(v), true
}

// RenderCommand renders a tool node's `command:` (or its `postcondition:`)
// as the executor does before `bash -c`: each reference shell-escaped, a
// reference that resolves to nothing kept as written so the shell sees it,
// and said to unresolved when a caller listens — the rendered text is not
// the place to look for it, a value may carry braces of its own.
//
// shapes carries the workflow's own declarations (DeclaredShapes) so a
// `json` value renders as one token and a `string[]` as argv words, exactly
// as the run will render them; nil renders every reference undeclared.
func RenderCommand(command string, refs []*ir.Ref, input, vars map[string]any, td *TemplateData, runID string, shapes *Shapes, unresolved func(ref string)) string {
	return renderCommand(command, refs, input, vars, td, runID, nil, shapes, unresolved)
}

// RenderScript renders a tool node's `script:` as the executor does before
// the interpreter: each reference a JSON literal, a reference that resolves
// to nothing the language's null — and said to unresolved when a caller
// listens, since the null hides it from the text.
func RenderScript(script string, refs []*ir.Ref, input, vars map[string]any, td *TemplateData, runID string, unresolved func(ref string)) string {
	return renderScript(script, refs, input, vars, td, runID, nil, unresolved)
}
