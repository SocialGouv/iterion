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

// resolveTemplate substitutes {{...}} references in a prompt body.
// td carries the runtime state for cross-namespace refs; pass nil
// to limit resolution to `input.*` and `vars.*`.
func (e *ClawExecutor) resolveTemplate(body string, input map[string]any, td *TemplateData) string {
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
		val, resolved := e.resolveTemplateRef(ref, input, td)
		if resolved {
			b.WriteString(val)
		} else {
			// Keep unresolved refs as-is.
			b.WriteString(remaining[start:end])
		}

		remaining = remaining[end:]

		// Guard against excessive expansion from large input values.
		// Truncate at the limit rather than appending the remaining template.
		if b.Len() > maxTemplateExpansionSize {
			e.logger.Warn("template expansion exceeded %d bytes, truncating", maxTemplateExpansionSize)
			break
		}
	}

	return b.String()
}

// resolveTemplateRef resolves a single "namespace.path" reference.
// Returns the resolved value and true, or ("", false) if unresolvable.
// Supported namespaces:
//   - input.<field>                                  — current node's input
//   - vars.<name>                                    — workflow variables
//   - outputs.<node_id>[.<field>...]                 — upstream node output
//   - loop.<name>.iteration                          — current iteration counter
//   - loop.<name>.max                                — declared loop bound
//   - loop.<name>.previous_output[.<field>...]       — snapshot one iteration behind
//   - artifacts.<publish_name>[.<field>...]          — published artifact
//   - run.id                                         — current run ID
//
// Cross-namespace refs require td (TemplateData) — when td is nil they
// resolve as not-found and the literal placeholder is preserved.
func (e *ClawExecutor) resolveTemplateRef(ref string, input map[string]any, td *TemplateData) (string, bool) {
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
			return formatValue(v), true
		}
	case "vars":
		if e.vars != nil {
			if v, ok := e.vars[key]; ok {
				return formatValue(v), true
			}
		}
	case "secrets":
		// {{secrets.X}} renders the opaque placeholder (Layer 1); the
		// real value is materialised by the secret guard at tool/shell
		// execution. File secrets render their mounted path. With the
		// placeholders kill-switch off value secrets render the real value
		// directly.
		if e.secretGuard != nil {
			name := key
			if dot := strings.IndexByte(key, '.'); dot >= 0 {
				name = key[:dot]
			}
			if v := e.secretGuard.ResolveSecretRef(name); v != "" {
				return v, true
			}
		}
	case "outputs":
		if td == nil {
			return "", false
		}
		segs := strings.Split(key, ".")
		nodeOut, ok := td.Outputs[segs[0]]
		if !ok || nodeOut == nil {
			return "", false
		}
		if len(segs) == 1 {
			return formatValue(nodeOut), true
		}
		v, ok := drillTemplatePath(nodeOut, segs[1:])
		if !ok {
			return "", false
		}
		return formatValue(v), true
	case "loop":
		if td == nil {
			return "", false
		}
		segs := strings.Split(key, ".")
		if len(segs) < 2 {
			return "", false
		}
		loopName, field := segs[0], segs[1]
		switch field {
		case "iteration":
			return formatValue(int64(td.LoopCounters[loopName])), true
		case "max":
			return formatValue(int64(td.LoopMaxIterations[loopName])), true
		case "previous_output":
			prev := td.LoopPreviousOutput[loopName]
			// Render empty string on the first iteration (prev is nil)
			// so prompts that say "vide si premiere iteration" read
			// naturally instead of leaving a literal placeholder.
			if len(segs) == 2 {
				if prev == nil {
					return "", true
				}
				return formatValue(prev), true
			}
			if prev == nil {
				return "", true
			}
			v, ok := drillTemplatePath(prev, segs[2:])
			if !ok {
				return "", true
			}
			return formatValue(v), true
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
			return formatValue(art), true
		}
		v, ok := drillTemplatePath(art, segs[1:])
		if !ok {
			return "", false
		}
		return formatValue(v), true
	case "run":
		return lookupRunTemplateRef(td, key)
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
			url, err := info.URL()
			if err != nil {
				return "", true
			}
			return url, true
		case "mime":
			return info.MIME, true
		case "size":
			return formatValue(info.Size), true
		case "sha256":
			return info.SHA256, true
		}
	}

	return "", false
}

// lookupRunTemplateRef resolves one `{{run.<key>}}` reference against the
// engine's namespace snapshot. Shared by the prompt path (resolveTemplateRef
// above) and the tool-command path (resolveRunRefs), so a member added to
// the namespace reaches both instead of rendering as a literal placeholder
// in whichever one was forgotten.
//
// `id` is served from RunID whether or not the snapshot map is populated:
// callers that predate the map (tests, hosts that wire only WithRunID) keep
// the member that has always worked. Any other unknown key stays
// unresolved, which is what a caller distinguishes from an empty value.
func lookupRunTemplateRef(td *TemplateData, key string) (string, bool) {
	if td == nil {
		return "", false
	}
	if key == "id" {
		return td.RunID, true
	}
	v, ok := td.Run[key]
	if !ok {
		return "", false
	}
	return formatValue(v), true
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

// prependPriorAskUser injects an explicit "[PRIOR INTERACTION]" block
// at the top of userText when the runtime relayed a prior ask_user
// question and answer. Returns userText unchanged when no relay is
// present (first invocation, or pause came from another source).
func prependPriorAskUser(userText string, input map[string]any) string {
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
