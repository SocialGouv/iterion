package ir

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
)

// secretRefVerdict judges one `{{secrets.X}}` reference against the
// workflow's declarations at a materialisation sink: the diagnostic code and
// the sentence, without the location the caller owns (an empty code is
// nothing to say). The compiler's own walk reads it (validateSecretsRef) and
// so do the readers of a text the walk never reached
// (PromptSecretRefIssues), so one reference can never collect two verdicts.
//
// grantRemedy is what the grant finding (C323) tells the author to do, the
// SITE's business: where the sink consumes the value's text, granting the
// sink is the remedy (secretGrantRemedy); where the sink needs a file — an
// LLM node's `images:` path — no grant makes a value usable and the remedy
// is the file form (imagePathSecretRemedy).
func secretRefVerdict(w *Workflow, ref *Ref, sink, grantRemedy string) (DiagCode, string) {
	if w == nil || ref == nil || len(ref.Path) == 0 {
		return "", ""
	}
	name := ref.Path[0]
	secret, ok := w.Secrets[name]
	if !ok {
		return DiagUnknownSecret, fmt.Sprintf("reference %s targets undeclared secret %q",
			expr.Excerpt(ref.Raw), expr.Excerpt(name))
	}
	if len(ref.Path) == 1 {
		// The grant check: a VALUE secret's reference materialises the value
		// at the sink the text feeds, so the secret must grant that sink. A
		// file secret's reference renders its mounted path wherever it
		// stands — the value reaches the run through the mount, which the
		// `file` grant on the declaration covers — so it is not checked per
		// reference. A sub-field reference is judged below first: one the
		// secret has not (C095) is refused for that alone.
		if sink != "" && !secret.IsFile() && !secret.Grants(sink) {
			return DiagSecretSinkUngranted, fmt.Sprintf(
				"reference %s reaches the %s sink, which secret %q does not grant (sinks: [%s]) — %s",
				expr.Excerpt(ref.Raw), sink, expr.Excerpt(name), strings.Join(secret.Sinks, ", "), grantRemedy)
		}
		return "", ""
	}
	if sub := ref.Path[1]; sub != "path" {
		return DiagSecretSubfield, fmt.Sprintf("reference %s uses unknown secret sub-field %q (expected: path)",
			expr.Excerpt(ref.Raw), expr.Excerpt(sub))
	}
	if !secret.IsFile() {
		return DiagSecretSubfield, fmt.Sprintf("reference %s uses .path on non-file secret %q",
			expr.Excerpt(ref.Raw), expr.Excerpt(name))
	}
	return "", ""
}

// secretGrantRemedy is the grant finding's remedy where the sink consumes
// the value's TEXT — a prompt body, a tool's command, an action's params:
// the declaration is what withholds it.
func secretGrantRemedy(sink string) string {
	return fmt.Sprintf("add `%s` to its sinks: list, or move the reference to a sink it grants", sink)
}

// imagePathSecretRemedy is the grant finding's remedy on an LLM node's
// `images:` path. The run renders the path through the prompt resolver and
// hands the result to the backend as a FILE to open (codex `-i`): a value
// secret renders as text there — its placeholder while placeholders are on,
// its bytes otherwise — and no grant turns text into a file the backend can
// read. Only a file secret's mounted path is one, so granting `prompt`, the
// remedy a prompt body gets, would buy a clean compile and a missing image.
func imagePathSecretRemedy(name string) string {
	return fmt.Sprintf("an image path is a file the backend opens, which a value secret's text never is: declare %q `as: file` and write `{{secrets.%s.path}}`, its mounted path", name, name)
}

// PromptSecretRefIssue is the verdict on one reference of a text the run
// renders at the PROMPT sink and the compile walk never reached — a bundle's
// `presets/<name>.md` body, folded into the workflow after compilation by
// runtime.MergeBundlePresets. The caller owns the location (the preset file)
// and composes it in front of Reason, as a compile diagnostic composes its
// own; Code is the compiler's code for the same defect, so the author reads
// one sentence and one remedy whichever surface says it.
type PromptSecretRefIssue struct {
	Code   DiagCode // C093, C095 or C323 on a reference; C004 on a text the parser cannot read
	Ref    string   // the reference as written; empty for C004, whose subject is the whole text
	Reason string   // what is wrong and the remedy, without the location
}

// PromptSecretRefIssues judges the `{{secrets.X}}` references of a text the
// run renders at the prompt sink, with the verdict the compiler gives the
// same reference in a prompt body. The other namespaces are not judged here:
// they resolve per NODE, against an input and outputs this walk has no node
// to read, and the run renders them as the prompt bodies it already checks.
//
// A text the template parser cannot read is one issue (C004): a single
// malformed delimiter hides every reference behind it, and silence there
// would read as "no reference". The parser's error quotes the rest of the
// text, so the reason is held to the compiler's own bound
// (expr.MaxDiagnosticText) before any surface — a lint message, a launch's
// log line — carries it. Repeated references draw one issue — the sentence
// is the same, and so is the fix.
//
// The grant and declaration verdicts are per READER, as the compiler's are
// (collectAllRefs reads a prompt once per node that names it, and a prompt
// no node names draws none): a workflow that builds no model task renders
// the text nowhere, so its references reach no sink and nothing is said
// (Workflow.RendersPresetPrompt). The template verdict stands regardless,
// as the compiler says C004 on a prompt's declaration whether or not a node
// reads it.
func PromptSecretRefIssues(w *Workflow, text string) []PromptSecretRefIssue {
	if w == nil || !strings.Contains(text, "{{") {
		return nil
	}
	refs, err := ParseRefs(text)
	if err != nil {
		return []PromptSecretRefIssue{{Code: DiagBadTemplateRef, Reason: expr.Clip(err.Error(), expr.MaxDiagnosticText)}}
	}
	if !w.RendersPresetPrompt() {
		return nil
	}
	var out []PromptSecretRefIssue
	seen := map[string]bool{}
	for _, ref := range refs {
		if ref == nil || ref.Kind != RefSecrets {
			continue
		}
		code, reason := secretRefVerdict(w, ref, secretsink.Prompt, secretGrantRemedy(secretsink.Prompt))
		if code == "" || seen[string(code)+"\x00"+ref.Raw] {
			continue
		}
		seen[string(code)+"\x00"+ref.Raw] = true
		out = append(out, PromptSecretRefIssue{Code: code, Ref: ref.Raw, Reason: reason})
	}
	return out
}
