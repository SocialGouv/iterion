package model

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// ---------------------------------------------------------------------------
// Template resolution
// ---------------------------------------------------------------------------

// buildUserMessage constructs the user message for an LLM call.
// userPrompt is the prompt reference name from the node (empty if not set).
// td carries the runtime state for cross-namespace refs (`outputs.*`,
// `loop.*`, `artifacts.*`, `run.*`); pass nil to skip those.
func (e *ClawExecutor) buildUserMessage(userPrompt string, input map[string]any, td *TemplateData) string {
	// If the node has a user prompt template, resolve it.
	if userPrompt != "" {
		if p, ok := e.prompts[userPrompt]; ok {
			return e.resolveTemplate(p.Body, input, td)
		}
	}

	// Fallback: serialize input as the user message.
	if len(input) == 0 {
		return ""
	}

	b, err := json.Marshal(input)
	if err != nil {
		return fmt.Sprintf("%v", input)
	}
	return string(b)
}

// buildUserContent extends buildUserMessage with multimodal output for
// backends that support image inputs (claw). When the resolved prompt
// references {{attachments.<name>}} (or .path) for an image-typed
// attachment, the helper splits the prompt around that reference and
// emits a separate ContentBlock carrying the image bytes, leaving the
// rest of the text intact.
//
// Single-pass: walks the prompt body once and builds the textual
// fallback AND the multimodal blocks in lockstep. Returns (text, nil)
// when no image was actually inlined, so the caller falls back to
// UserPrompt without bothering with multimodal wrapping.
func (e *ClawExecutor) buildUserContent(
	userPrompt string,
	input map[string]any,
	td *TemplateData,
	imageAttachments map[string]bool,
) (string, []delegate.ContentBlock) {
	if userPrompt == "" || td == nil || len(td.Attachments) == 0 || len(imageAttachments) == 0 {
		return e.buildUserMessage(userPrompt, input, td), nil
	}
	p, ok := e.prompts[userPrompt]
	if !ok {
		return e.buildUserMessage(userPrompt, input, td), nil
	}

	var (
		text   strings.Builder
		blocks []delegate.ContentBlock
		buf    strings.Builder // accumulates text since last image block
		body   = p.Body
		hasImg = false
	)
	flush := func() {
		if buf.Len() == 0 {
			return
		}
		blocks = append(blocks, delegate.ContentBlock{Type: "text", Text: buf.String()})
		buf.Reset()
	}

	for {
		start := strings.Index(body, "{{")
		if start == -1 {
			text.WriteString(body)
			buf.WriteString(body)
			break
		}
		// Static prefix before the next placeholder.
		text.WriteString(body[:start])
		buf.WriteString(body[:start])

		end := strings.Index(body[start:], "}}")
		if end == -1 {
			text.WriteString(body[start:])
			buf.WriteString(body[start:])
			break
		}
		end += start + 2
		ref := strings.TrimSpace(body[start+2 : end-2])

		if isImageAttachmentRef(ref, imageAttachments) {
			info, infoOK := td.Attachments[attachmentRefName(ref)]
			if infoOK {
				if blk, err := e.imageContentBlock(info); err == nil {
					flush()
					blocks = append(blocks, blk)
					text.WriteString(info.Path)
					hasImg = true
					body = body[end:]
					continue
				}
				// Failed to load bytes — interpolate as text path so
				// the agent can still reach the file via read_image.
				text.WriteString(info.Path)
				buf.WriteString(info.Path)
				body = body[end:]
				continue
			}
		}
		val, resolved := e.resolveTemplateRef(ref, input, td)
		if resolved {
			text.WriteString(val)
			buf.WriteString(val)
		} else {
			text.WriteString(body[start:end])
			buf.WriteString(body[start:end])
		}
		body = body[end:]
	}
	if !hasImg {
		return text.String(), nil
	}
	flush()
	return text.String(), blocks
}

// isImageAttachmentRef reports whether the given template reference
// (without the "{{" "}}" delimiters) targets an image attachment whose
// rendered position should become a separate ContentBlock. Matches the
// default form `attachments.<name>` and the explicit `attachments.<name>.path`.
func isImageAttachmentRef(ref string, imageNames map[string]bool) bool {
	parts := strings.Split(ref, ".")
	if len(parts) < 2 || parts[0] != "attachments" {
		return false
	}
	if len(parts) >= 3 && parts[2] != "path" {
		return false
	}
	return imageNames[parts[1]]
}

func attachmentRefName(ref string) string {
	parts := strings.Split(ref, ".")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// imageContentBlock loads the bytes for an image attachment and
// builds a base64-inline ContentBlock. Files larger than
// imageInlineByteLimit fall back to a URL block so the LLM API
// receives a remote URL instead of an oversized payload.
const imageInlineByteLimit = 5 * 1024 * 1024 // 5 MiB

func (e *ClawExecutor) imageContentBlock(info AttachmentInfo) (delegate.ContentBlock, error) {
	// The bytes are read by THIS process, on the host. info.Path is the
	// path the NODES see — the sandbox bind-mount path on a containerised
	// run, which does not exist out here — so inlining must go through
	// HostPath. Falling back to Path keeps unsandboxed runs and callers
	// that only populate one field working.
	hostPath := info.HostPath
	if hostPath == "" {
		hostPath = info.Path
	}
	if hostPath == "" {
		// No local bytes available — emit a URL block when the
		// store can presign one, otherwise return an error so the
		// caller falls back to text.
		url, err := info.URL()
		if err != nil || url == "" {
			return delegate.ContentBlock{}, fmt.Errorf("attachment %q: no local path or URL", info.Name)
		}
		return delegate.ContentBlock{
			Type:      "image",
			MediaType: info.MIME,
			URL:       url,
			Path:      info.Path,
			Name:      info.Name,
		}, nil
	}
	if info.Size > imageInlineByteLimit {
		url, err := info.URL()
		if err == nil && url != "" {
			return delegate.ContentBlock{
				Type:      "image",
				MediaType: info.MIME,
				URL:       url,
				Path:      info.Path,
				Name:      info.Name,
			}, nil
		}
		// No URL backend — fall through and inline anyway. The
		// runtime will surface the API's size error to the user.
	}
	body, err := os.ReadFile(hostPath)
	if err != nil {
		return delegate.ContentBlock{}, fmt.Errorf("read image %q: %w", hostPath, err)
	}
	return delegate.ContentBlock{
		Type:      "image",
		MediaType: info.MIME,
		Data:      base64.StdEncoding.EncodeToString(body),
		Path:      info.Path,
		Name:      info.Name,
	}, nil
}

// maxTemplateExpansionSize is the maximum allowed size of a resolved template.
// Prevents OOM from extremely large input values injected into prompts.
const maxTemplateExpansionSize = 5 * 1024 * 1024 // 5 MB

// templateResolver is the executor's view of the shared renderer: its
// vars, its secret guard when one is wired, its logger for the expansion
// warning. Unresolved references stay silent here — the model reads the
// placeholder — a dry run listens to them (TemplateResolver.Unresolved).
func (e *ClawExecutor) templateResolver() *TemplateResolver {
	r := &TemplateResolver{Vars: e.vars, Warn: func(format string, args ...any) {
		if e.logger != nil {
			e.logger.Warn(format, args...)
		}
	}}
	if e.secretGuard != nil {
		r.Secrets = e.secretGuard
	}
	return r
}

// resolveTemplate substitutes {{...}} references in a prompt body.
// td carries the runtime state for cross-namespace refs; pass nil
// to limit resolution to `input.*` and `vars.*`.
func (e *ClawExecutor) resolveTemplate(body string, input map[string]any, td *TemplateData) string {
	return e.templateResolver().Resolve(body, input, td)
}

// resolveTemplateRef resolves a single "namespace.path" reference —
// TemplateResolver.ResolveRef with the executor's vars and secret guard.
func (e *ClawExecutor) resolveTemplateRef(ref string, input map[string]any, td *TemplateData) (string, bool) {
	return e.templateResolver().ResolveRef(ref, input, td)
}

// outputsTemplateValue resolves one `outputs.<node>[.<field>…]` reference to
// its RAW value from the template snapshot — the single lookup behind the
// prompt path (resolveTemplateRef, which formats it) and the tool command /
// script / postcondition path (resolveTemplateWith, which shell-escapes or
// JSON-encodes it), so an output a prompt can read cannot stay a literal in
// a command. Unresolved — no snapshot, a node that has not produced, a field
// the output does not carry — is reported as such; each caller applies its
// own missing-value rule.
func outputsTemplateValue(td *TemplateData, segs []string) (any, bool) {
	if td == nil || len(segs) == 0 {
		return nil, false
	}
	nodeOut, ok := td.Outputs[segs[0]]
	if !ok || nodeOut == nil {
		return nil, false
	}
	if len(segs) == 1 {
		return nodeOut, true
	}
	return drillTemplatePath(nodeOut, segs[1:])
}

// lookupRunTemplateRef resolves one `{{run.<key>}}` reference for a PROMPT
// body, formatting the raw value runNamespaceValue returns. The tool
// command / script / postcondition path (resolveTemplateWith) reads the
// same lookup with its own renderer, so a member added to the namespace
// reaches both instead of rendering as a literal placeholder in whichever
// one was forgotten.
//
// `id` is served from RunID whether or not the snapshot's Run map is
// populated: callers that predate the map (tests, hosts that wire only
// WithRunID) keep the member that has always worked. Any other unknown key
// stays unresolved, which is what a caller distinguishes from an empty value.
func lookupRunTemplateRef(td *TemplateData, key string) (string, bool) {
	v, ok := runNamespaceValue("", td, key)
	if !ok {
		return "", false
	}
	return formatValue(v), true
}

// runNamespaceValue resolves one `run.<member>` to its RAW value — the
// single lookup behind both the prompt path (lookupRunTemplateRef, which
// formats it) and the tool command / script / postcondition path
// (resolveTemplateWith, which shell-escapes or JSON-encodes it), so a
// member cannot resolve in one and stay literal in the other.
//
// The TEMPLATE SNAPSHOT is the authority, including for `id`: it is the
// only source a fan-out branch has, since the engine withholds the ctx run
// identity there (a key that would alias sibling items — see pkg/runtime's
// execContext). ctxRunID is the fallback for `id` alone, for hosts that
// wire WithRunID and no snapshot. `id` resolves to the empty string rather
// than to its own placeholder whenever either is wired. A member neither
// source carries is reported unresolved; each caller applies its own
// missing-value rule.
func runNamespaceValue(ctxRunID string, td *TemplateData, member string) (any, bool) {
	if member == "id" {
		if td != nil && td.RunID != "" {
			return td.RunID, true
		}
		if ctxRunID != "" {
			return ctxRunID, true
		}
		if td != nil {
			return "", true
		}
		return nil, false
	}
	if td == nil {
		return nil, false
	}
	v, ok := td.Run[member]
	return v, ok
}

// drillTemplatePath walks a dotted path through nested maps. Returns
// the leaf value and true on success, or (nil, false) when any segment
// can't be resolved. Used by resolveTemplateRef to drill into
// outputs.<node>.<field>, loop.<name>.previous_output.<field>, etc.
func drillTemplatePath(root map[string]any, path []string) (any, bool) {
	if len(path) == 0 {
		return root, true
	}
	var cur any = root
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := m[p]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// formatValue converts an interface value to a string for template substitution.
func formatValue(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case nil:
		return ""
	default:
		b, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(b)
	}
}

// askUserToolName is the qualified name under which iterion registers
// claw-code-go's native ask_user tool. Kept private to model so
// nothing else hard-codes the string.
const askUserToolName = "ask_user"

// ensureToolPresent returns tools with `name` appended if not already
// present. The returned slice is a fresh defensive copy when an append
// happens, so the caller's slice header is never aliased.
//
// Callers:
//   - ensureAskUser  (askUserToolName): guarantees a node with
//     interaction enabled exposes a way to escalate to the human.
//   - ensureAgentTool ("agent"): keeps the claw subagent tool reachable
//     for ultracode nodes that restrict their tool set.
//   - ensureReadImage ("read_image"): lets CLI-based backends
//     (claude_code, codex) reach image attachments via their vision tool.
//
// Idempotent.
func ensureToolPresent(tools []string, name string) []string {
	if slices.Contains(tools, name) {
		return tools
	}
	return append(append([]string(nil), tools...), name)
}

// promptReferencesImage returns true when promptName resolves to a
// prompt body containing a {{attachments.<name>}} reference where
// <name> is in imageNames. Used to decide whether the CLI-backend
// fallback should auto-enable read_image.
func promptReferencesImage(promptName string, prompts map[string]*ir.Prompt, imageNames map[string]bool) bool {
	if promptName == "" || len(imageNames) == 0 {
		return false
	}
	p, ok := prompts[promptName]
	if !ok {
		return false
	}
	body := p.Body
	for {
		i := strings.Index(body, "{{")
		if i < 0 {
			return false
		}
		j := strings.Index(body[i:], "}}")
		if j < 0 {
			return false
		}
		ref := strings.TrimSpace(body[i+2 : i+j])
		parts := strings.Split(ref, ".")
		if len(parts) >= 2 && parts[0] == "attachments" && imageNames[parts[1]] {
			return true
		}
		body = body[i+j+2:]
	}
}

// sameStringSlice reports element-wise equality.
func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// prependPriorAskUser restores the human answer that caused a backend
// re-invocation. Native ask_user keeps its explicit reminder framing. A custom
// `_interaction_questions` answer is placed verbatim first: it may itself be a
// Studio capability payload whose protocol markers are valid only at the start
// of the operator message (for example <active-editor-document>).
func prependPriorAskUser(userText string, input map[string]any) string {
	if questions, ok := input[delegate.PriorInteractionQuestionsKey].(map[string]any); ok {
		if answers, ok := input[delegate.PriorInteractionAnswersKey].(map[string]any); ok {
			if key, question, answer, ok := soleStringInteraction(questions, answers); ok {
				reminder := systemReminder(fmt.Sprintf("[PRIOR INTERACTION]\nYou requested human input under field %q: %q\nThe exact operator answer appears above this reminder. Use it to complete the task and do not ask the same question again.", key, question))
				return answer + "\n\n" + reminder + "\n\n" + userText
			}
			q, qErr := json.Marshal(questions)
			a, aErr := json.Marshal(answers)
			if qErr == nil && aErr == nil {
				reminder := systemReminder(fmt.Sprintf("[PRIOR INTERACTION]\nYou requested human input: %s\nThe user answered: %s\nUse these answers to complete the task and do not ask the same questions again.", q, a))
				return reminder + "\n\n" + userText
			}
		}
	}
	q, qOK := input[delegate.PriorAskUserQuestionKey].(string)
	if !qOK || q == "" {
		return userText
	}
	a, _ := input[delegate.PriorAskUserAnswerKey].(string)
	// A permission `ask` pause (claude_code) is not an ask_user call: the
	// model tried a tool, the gate suspended it, and the operator
	// authorized (GrantInputKey set) or denied it. Frame the resume so the
	// model re-issues the now-authorized call (or adapts on denial).
	// The harness context is wrapped in <system-reminder> so the model reads
	// it as injected state, cleanly separated from the user text it precedes;
	// the bracket labels stay as stable transcript markers.
	if grant, ok := input[permission.GrantInputKey].(string); ok && grant != "" {
		return systemReminder(fmt.Sprintf("[PERMISSION GRANTED]\nThe operator approved your previous tool call (%s). It is now authorized — re-issue the exact same tool call now to perform it.", q)) + "\n\n" + userText
	}
	if isPermissionPrompt(q) {
		return systemReminder(fmt.Sprintf("[PERMISSION DENIED]\nThe operator denied your previous tool call (%s). Do not retry it; take a different approach or explain why it is needed.", q)) + "\n\n" + userText
	}
	return systemReminder(fmt.Sprintf("[PRIOR INTERACTION]\nYou previously called ask_user with question: %q\nThe user answered: %q\nUse this answer to complete your task. Do NOT call ask_user with the same question again.", q, a)) + "\n\n" + userText
}

func soleStringInteraction(questions, answers map[string]any) (key, question, answer string, ok bool) {
	for candidate, rawQuestion := range questions {
		if strings.HasPrefix(candidate, "_") {
			continue
		}
		q, qOK := rawQuestion.(string)
		a, aOK := answers[candidate].(string)
		if !qOK || !aOK || q == "" || a == "" || ok {
			return "", "", "", false
		}
		key, question, answer, ok = candidate, q, a, true
	}
	return key, question, answer, ok
}

// isPermissionPrompt reports whether a relayed prior question is a
// permission-gate approval prompt (vs an ask_user clarifying question).
func isPermissionPrompt(q string) bool {
	return strings.HasPrefix(q, permission.AskPromptPrefix)
}

// redactJSONTextField returns a sanitized copy of a JSON object
// with the `text` field replaced by privacy.EventTextMarker. Other
// fields (mode, categories, substituted, missing, ...) are
// preserved so operators can still see how the call was
// parameterised and which placeholders the unfilter saw. Decode
// failure or absent `text` field → input returned unchanged
// (best-effort: a malformed payload is already going to surface
// via the tool's own error path).
func redactJSONTextField(in []byte) []byte {
	if len(in) == 0 {
		return in
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(in, &m); err != nil {
		return in
	}
	if _, ok := m["text"]; !ok {
		return in
	}
	body, err := json.Marshal(privacy.EventTextMarker)
	if err != nil {
		return in
	}
	m["text"] = body
	out, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return out
}
