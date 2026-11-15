package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A shell `script:` (sh, bash) is read by a shell, which expands `$(…)`,
// `$VAR`, `${…}` and backquotes wherever it reads and decides where a quote
// opens or closes — a decision no lexer of the engine's can be sure to share
// with every shell (#3291). So data never enters a shell script's text: each
// data reference (ir.ShellScriptData) renders to a read of a variable named
// for this one rendering (`__ITERION_DATA_<marker>_<n>__`, the marker drawn
// from crypto/rand), and the value rides the interpreter's environment, as a
// secret's does (bodySecrets). A parameter expansion's result is never parsed
// again by the shell, and every read the renderer writes is balanced for any
// quoting it lands in, so where it stands decides only whether the value is
// word-split, never whether it runs:
//
//   - inside the author's double quotes, or inside arithmetic, `${NAME}`;
//   - in the body of a heredoc whose delimiter is unquoted, `${NAME_JSON}`,
//     a second variable holding the value's JSON literal — what that body
//     has always received, the payload idiom (`cat <<EOF` of a JSON
//     document) — expanded there and read again by nothing;
//   - after an unescaped backslash, `${NAME}`: the escaped `$` keeps its
//     text, and no quote of the rendering is the one escaped;
//   - everywhere else, `"${NAME}"`: one word, never split.
//
// The value is the raw form's text (rawTemplateValue): a string as it is, a
// number or a boolean its JSON, an object or a list its compact JSON text —
// ONE value, never one word per element — and `null` for a hole. A value
// holding a NUL byte is refused, naming the reference: neither the
// environment nor a shell variable can carry it.
//
// The value rides the environment while it fits (dataEnvValueMax, and
// dataEnvTotalMax over all of a script's data); past that, the script opens
// with a single-quoted assignment of the variable (scriptData.deliver), the
// engine's own text ahead of the author's, which no construct of the
// author's can precede. Such a value is a variable of the script and not of
// its environment: the processes the script starts do not inherit it.
//
// A secret reference, the raw `{{!…}}` form and `{{"{{"}}` are not data:
// they render as in any script body — the raw form is the author's own text,
// spliced as written, the author's deliberate opt-out.

// dataEnvValueMax bounds one value delivered through the environment, and
// dataEnvTotalMax all of a script's: Linux refuses one environment string
// past 128 KiB (MAX_ARG_STRLEN) and the whole of argv and envp past ARG_MAX,
// which every process the script starts inherits too.
const (
	dataEnvValueMax = 64 << 10
	dataEnvTotalMax = 256 << 10
)

// renderScriptIn renders a tool node's `script:` written in language: a
// shell script through renderShellScript, any other language through
// renderScript (every reference a JSON literal). mark is resolveTemplateWith's
// (bodySecrets). The scriptData is what a shell script's interpreter must
// receive for its data references; the error names a reference no form
// carries (renderShellScript).
func renderScriptIn(script, language string, refs []*ir.Ref, input, vars map[string]any, td *TemplateData, ctxRunID string, guard *secretguard.Guard, unresolved func(ref string), mark func(text string) string) (string, scriptData, error) {
	if scriptRunsInAShell(language) {
		return renderShellScript(script, language == "bash", refs, input, vars, td, ctxRunID, guard, unresolved, mark)
	}
	return renderScript(script, refs, input, vars, td, ctxRunID, guard, unresolved, mark), scriptData{}, nil
}

// renderShellScript renders a sh (bash false) or bash script: each data
// reference as a read of its variable, written for the quoting its position
// reads (ir.ShellScriptRefPositions), its value resolved by the shared core
// (resolveTemplateWith, which defuses a placeholder data spells) into the
// returned scriptData; every other reference as any script body renders it.
// The walk takes the shared core's `{{…}}` spans.
func renderShellScript(script string, bash bool, refs []*ir.Ref, input, vars map[string]any, td *TemplateData, ctxRunID string, guard *secretguard.Guard, unresolved func(ref string), mark func(text string) string) (string, scriptData, error) {
	var data scriptData
	if len(refs) == 0 {
		return script, data, nil
	}
	byRaw := make(map[string]*ir.Ref, len(refs))
	for _, ref := range refs {
		byRaw[ref.Raw] = ref
	}
	positions := ir.ShellScriptRefPositions(script, bash)
	marker := drawDataMarker()
	index := map[string]int{}
	var b strings.Builder
	b.Grow(len(script))
	last := 0
	for _, span := range ir.TemplateSpans(script) {
		b.WriteString(script[last:span[0]])
		last = span[1]
		raw := script[span[0]:span[1]]
		ref, ok := byRaw[raw]
		if !ok {
			b.WriteString(raw)
			continue
		}
		if !ir.ShellScriptData(ref) || previewMask(ref, vars, guard) {
			b.WriteString(resolveTemplateWith(raw, []*ir.Ref{ref}, input, vars, td, ctxRunID, guard, nil, false, jsonLiteralValue, true, unresolved, mark))
			continue
		}
		n, seen := index[raw]
		if !seen {
			n = len(index) + 1
			index[raw] = n
		}
		name := "__ITERION_DATA_" + marker + "_" + strconv.Itoa(n) + "__"
		pos := positions[span[0]]
		value := rawTemplateValue
		if pos.Context == ir.ShellRefHeredoc && !pos.Unsure {
			name, value = strings.TrimSuffix(name, "__")+"_JSON__", jsonLiteralValue
		}
		text := resolveTemplateWith(raw, []*ir.Ref{ref}, input, vars, td, ctxRunID, guard, nil, false, value, true, unresolved, nil)
		if strings.ContainsRune(text, 0) {
			return "", scriptData{}, fmt.Errorf("the value of %s holds a NUL byte, which a shell script can carry neither in its environment nor in a variable", raw)
		}
		data.set(name, text)
		b.WriteString(shellDataRead(name, pos, gluedToBackslash(script, span[0])))
	}
	b.WriteString(script[last:])
	return b.String(), data, nil
}

// previewMask reports whether ref is a server-minted credential's
// `{{vars.x}}` in a rendering with no guard behind it — a preview, which
// renders the credential's placeholder (mintedVarMask), a token of the
// engine's and no data. A run refuses such a reference in a script
// (refuseMintedScriptRef).
func previewMask(ref *ir.Ref, vars map[string]any, guard *secretguard.Guard) bool {
	if guard != nil || ref.Kind != ir.RefVars || len(ref.Path) != 1 {
		return false
	}
	_, masked := mintedVarMask(ref.Path[0], vars[ref.Path[0]])
	return masked
}

// shellDataRead is the read of the variable name written for pos (the
// reading above renderScriptIn).
func shellDataRead(name string, pos ir.ShellRefPosition, glued bool) string {
	switch {
	case glued:
		return "${" + name + "}"
	case pos.Unsure:
		return `"${` + name + `}"`
	case pos.Context == ir.ShellRefDouble || pos.Context == ir.ShellRefArith || pos.Context == ir.ShellRefHeredoc:
		return "${" + name + "}"
	}
	return `"${` + name + `}"`
}

// gluedToBackslash reports whether the byte before at is a backslash that is
// not itself escaped: it would escape the first byte the rendering writes.
func gluedToBackslash(s string, at int) bool {
	n := 0
	for at-n-1 >= 0 && s[at-n-1] == '\\' {
		n++
	}
	return n%2 == 1
}

// drawDataMarker is one rendering's marker for its data variables.
func drawDataMarker() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("model: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// scriptData is what a shell script's data references hand its interpreter:
// each variable the rendering names, with its value, in the order drawn.
type scriptData struct {
	names  []string
	values map[string]string
}

func (d *scriptData) set(name, value string) {
	if d.values == nil {
		d.values = map[string]string{}
	}
	if _, ok := d.values[name]; !ok {
		d.names = append(d.names, name)
	}
	d.values[name] = value
}

// deliver splits d into the environment the interpreter starts with and the
// prologue the script opens with: each value in the environment while it
// fits dataEnvValueMax and the running total dataEnvTotalMax, the rest
// assigned by the prologue as single-quoted words (shellEscape) — text the
// engine writes ahead of the author's, so no construct of the author's
// precedes or encloses it.
func (d scriptData) deliver() (map[string]string, string) {
	if len(d.names) == 0 {
		return nil, ""
	}
	env := make(map[string]string, len(d.names))
	var prologue strings.Builder
	total := 0
	for _, name := range d.names {
		value := d.values[name]
		entry := len(name) + len(value) + 2
		if len(value) <= dataEnvValueMax && total+entry <= dataEnvTotalMax {
			env[name] = value
			total += entry
			continue
		}
		prologue.WriteString(name + "=" + shellEscape(value) + "\n")
	}
	return env, prologue.String()
}
