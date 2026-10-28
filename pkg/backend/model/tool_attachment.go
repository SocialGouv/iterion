package model

import (
	"context"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// toolAttachmentDirective is the line prefix a tool node uses to hand a
// file it produced to the run, so a downstream human gate can SHOW it.
//
// Shape:
// `[iterion] attachment=<path> [name=<n>] [mime=<type/subtype>] [required=true]`
//
// Why this exists. A gate renders its inbound payload (iterion#332), and
// a `file`-typed value is previewed by fetching
// `GET /api/runs/{id}/attachments/{name}` — the descriptor's path is a
// host or bind-mount path and is never reachable from a browser. So a
// workflow that GENERATES a deliverable (a rendered video, an audio mix,
// a chart) had no way to put it in front of the reviewer: attachments
// only entered a run at launch or from an operator's own upload.
//
// This is the writing counterpart of `preview_screenshot=`, which
// already promotes a browser capture the same way. Same protocol, same
// store API, no new surface: a tool prints one line per file, the
// runtime reads the bytes and persists them.
const toolAttachmentDirective = "[iterion] attachment="

// maxToolAttachmentBytes bounds what a directive may copy into the run
// store. FilesystemRunStore.WriteAttachment documents the cap as the
// caller's job and simply io.Copy's the body, so without this a
// directive naming a 40 GB render would fill the store silently. The
// value matches the cloud store's own default, so a bot behaves the
// same locally and in cloud mode.
const maxToolAttachmentBytes = 50 * 1024 * 1024

// AttachmentDirective is one parsed `[iterion] attachment=...` line.
// Path is host-absolute. Name defaults to the file's base name,
// sanitised; MIME is sniffed from the extension when absent.
type AttachmentDirective struct {
	Path string
	Name string
	MIME string
	// Required turns the loss of this file into the NODE's failure
	// instead of an event beside its success.
	//
	// Opt-in, and the default is unchanged: a tool's own output is the
	// contract with the workflow, so a deliverable that did not reach a
	// human is not by itself a reason to fail the step — every bot that
	// declares a conditional or best-effort `attachment=` keeps running
	// exactly as before. A workflow whose POINT is the file (an audit
	// report, a signed export) says so with `required=true` and gets a
	// failing node rather than a success nobody can collect (#2668).
	Required bool
}

// attachmentDirectiveKeys are the trailing `key=value` tokens the
// directive accepts. Anything else belongs to the PATH — see
// splitAttachmentTail.
var attachmentDirectiveKeys = []string{"name", "mime", "required"}

// splitAttachmentTail separates the path from the trailing options.
//
// The shared scanner splits on whitespace, which breaks the moment a
// path contains a space — and exported media routinely does
// ("final cut.mp4"). The failure was the worst possible one: os.Open on
// a truncated path, a warning, and the file skipped — exactly the empty
// gate this verb exists to prevent. So the path is everything up to the
// first recognised ` key=` token.
func splitAttachmentTail(tokens []string) (string, []string) {
	for index := 1; index < len(tokens); index++ {
		for _, key := range attachmentDirectiveKeys {
			if strings.HasPrefix(tokens[index], key+"=") {
				return strings.Join(tokens[:index], " "), tokens[index:]
			}
		}
	}
	return strings.Join(tokens, " "), nil
}

// scanToolAttachments walks tool stdout and returns one directive per
// matching line. Reading the file and persisting it is the runtime
// hook's job, exactly as for screenshots.
func scanToolAttachments(output string) []AttachmentDirective {
	lines := scanDirectiveLines(output, toolAttachmentDirective)
	if len(lines) == 0 {
		return nil
	}
	found := make([]AttachmentDirective, 0, len(lines))
	for _, tokens := range lines {
		path, options := splitAttachmentTail(tokens)
		dir := AttachmentDirective{Path: path}
		for _, kv := range options {
			eq := strings.IndexByte(kv, '=')
			if eq <= 0 {
				continue
			}
			switch kv[:eq] {
			case "name":
				dir.Name = kv[eq+1:]
			case "mime":
				dir.MIME = kv[eq+1:]
			case "required":
				dir.Required = directiveRequires(kv[eq+1:])
			}
		}
		found = append(found, dir)
	}
	return found
}

// directiveRequires reads the `required=` token. The usual boolean
// spellings answer (`true`/`false`, `1`/`0`, …); anything else — a typo,
// `yes`, an empty value — reads as TRUE, because the token is only ever
// typed to ASK for the guarantee: a value no parser recognises must not
// quietly cancel it. The opposite default would turn `required=yes` into
// the silent loss this directive exists to end.
func directiveRequires(value string) bool {
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return true
	}
	return parsed
}

// neutralizeActiveMIME downgrades any type the browser would EXECUTE
// when `/api/runs/{id}/attachments/{name}` serves it back with
// `Content-Disposition: inline` — that route sets neither nosniff nor a
// CSP, and accepts a request carrying no Origin header at all.
//
// A tool's stdout is not a trusted channel: it carries fetched pages,
// catted files and LLM-written text, any line of which can carry a
// directive. Without this, `mime=text/html` plants a same-origin script
// in the studio. The operator-upload path defends the same invariant by
// SNIFFING the bytes against AllowedUploadMIMEs; this path cannot sniff
// (it never reads the content) so it downgrades instead — the
// deliverable still reaches the gate, as a download rather than a
// preview.
func neutralizeActiveMIME(m string) string {
	switch strings.ToLower(m) {
	case "text/html", "application/xhtml+xml", "image/svg+xml",
		"text/xml", "application/xml",
		"text/javascript", "application/javascript", "application/x-javascript":
		return "application/octet-stream"
	}
	return m
}

// attachmentMIME resolves the stored MIME: the directive's own value
// when it is well formed, else a sniff from the extension. An unknown
// extension yields the generic binary type rather than a lie — the
// studio falls back to a download link, which is still usable.
func attachmentMIME(dir AttachmentDirective) string {
	if declared := strings.TrimSpace(dir.MIME); declared != "" {
		if parsed, _, err := mime.ParseMediaType(declared); err == nil && parsed != "" {
			return neutralizeActiveMIME(parsed)
		}
	}
	if byExt := mime.TypeByExtension(filepath.Ext(dir.Path)); byExt != "" {
		if parsed, _, err := mime.ParseMediaType(byExt); err == nil && parsed != "" {
			return neutralizeActiveMIME(parsed)
		}
	}
	return "application/octet-stream"
}

// AttachmentLister is the optional read half of the attachment
// capability. When the sink implements it too, publishToolAttachment
// refuses to overwrite a name the run already carries.
type AttachmentLister interface {
	ListAttachments(ctx context.Context, runID string) ([]store.AttachmentRecord, error)
}

// attachmentLossReason is the stable token EventRunAttachmentLost
// carries, so a reader can group losses without parsing prose.
type attachmentLossReason string

const (
	// lossUnreadable: the declared path could not be stat'd or opened
	// from here. THE cloud case: the file is in the pod, this is the
	// host (#2668).
	lossUnreadable attachmentLossReason = "unreadable"
	// lossNotAFile: the path is a directory or another non-regular file.
	lossNotAFile attachmentLossReason = "not_a_file"
	// lossOverCap: bigger than maxToolAttachmentBytes.
	lossOverCap attachmentLossReason = "over_cap"
	// lossNameTaken: the run already carries that handle, and a stable
	// name is the whole point of the verb — the newcomer is refused
	// rather than suffixed away.
	lossNameTaken attachmentLossReason = "name_taken"
	// lossStoreWriteFailed: the bytes were readable but the store
	// refused them.
	lossStoreWriteFailed attachmentLossReason = "store_write_failed"
)

// Directive tokens EventRunAttachmentLost reports under, matching the
// stdout verb the author wrote.
const (
	directiveAttachment        = "attachment"
	directivePreviewScreenshot = "preview_screenshot"
)

// attachmentLoss is one declared file the run did not receive.
type attachmentLoss struct {
	// Directive is the stdout verb that declared it.
	Directive string
	// Path is what the tool printed.
	Path string
	// Name is the handle it would have been served under, empty when
	// the loss happened before one was derived.
	Name   string
	Reason attachmentLossReason
	// Detail is the human cause, as the warning has always phrased it.
	Detail string
	// Required carries the directive's own `required=true`: this loss is
	// the node's failure, not a note beside its success. Only the
	// `attachment=` verb can set it — see recordAttachmentLost.
	Required bool
}

// requiredAttachmentLostError is the node failure a `required=true`
// directive asks for: the file the node declared never reached the run,
// so the step did not do what it says it does.
//
// Typed rather than a bare fmt.Errorf because the recipe SUCCEEDED —
// the ladder must not read it as a command to repair, and re-running
// cannot bring back bytes this process was never able to read (the pod
// case, #2668). It names the path and the reason, so the failure an
// operator reads is the one the run recorded.
type requiredAttachmentLostError struct{ loss attachmentLoss }

func (e *requiredAttachmentLostError) Error() string {
	return fmt.Sprintf(
		"required %s=%s was not delivered to the run (%s): %s",
		e.loss.Directive, e.loss.Path, e.loss.Reason, e.loss.Detail)
}

// recordAttachmentLost is the single exit for a declared file that did
// not make it: it warns on the runner's log AS BEFORE and records the
// loss on the RUN, where the operator and the studio can see it.
//
// One chokepoint rather than a warn+emit pair repeated at every drop
// site: both stdout file verbs lose files for the same five reasons,
// and the next drop site added next to a bare logger.Warn would be
// silent again — the defect this closes (#2668).
//
// It also returns the verdict: nil when the node keeps its own result
// (the default, non-fatal contract — the caller returns after this),
// and a requiredAttachmentLostError when the directive said
// `required=true`. Failing the node is OPT-IN and lives here, at the
// one place that knows a declared file is gone, so a verb added beside
// this chokepoint inherits both halves instead of half of them.
func recordAttachmentLost(
	ctx context.Context,
	emitter EventEmitter,
	runID, nodeID string,
	loss attachmentLoss,
	logger *iterlog.Logger,
) error {
	label := "Tool attachment"
	if loss.Directive == directivePreviewScreenshot {
		label = "Browser screenshot"
	}
	logger.Warn("%s [%s]: %s — not delivered to the run (%s)",
		label, nodeID, loss.Detail, loss.Reason)

	data := map[string]any{
		"directive": loss.Directive,
		"path":      loss.Path,
		"reason":    string(loss.Reason),
		"detail":    loss.Detail,
		"source":    "tool-stdout",
		// Always sent, both ways: a reader filtering the losses that
		// FAILED a node cannot tell a false from an absent field.
		"required": loss.Required,
	}
	// Omitted rather than sent empty: an empty handle would read as "it
	// was served under no name" instead of "no name was reached yet".
	if loss.Name != "" {
		data["name"] = loss.Name
	}
	_, _ = emitter.AppendEvent(ctx, runID, store.Event{
		Type:   store.EventRunAttachmentLost,
		RunID:  runID,
		NodeID: nodeID,
		Data:   data,
	})

	if !loss.Required {
		return nil
	}
	return &requiredAttachmentLostError{loss: loss}
}

// publishToolAttachment reads the file a tool declared and persists it
// as a run attachment, so the studio can fetch it through the existing
// `/api/runs/{id}/attachments/{name}` route.
//
// Failures are non-fatal BY DEFAULT: a missing or unreadable file must
// never abort a tool node that did not ask for the guarantee. The tool's
// own output is the contract with the workflow; the directive is how the
// bytes reach a human. They are not silent either — every exit that does
// not publish goes through recordAttachmentLost, so the run carries the
// loss. The returned error is that chokepoint's verdict: non-nil only
// for a directive that declared `required=true`, in which case the
// caller fails the node with it.
func publishToolAttachment(
	ctx context.Context,
	sink AttachmentWriter,
	emitter EventEmitter,
	runID, nodeID string,
	dir AttachmentDirective,
	logger *iterlog.Logger,
) error {
	info, err := os.Stat(dir.Path)
	if err != nil {
		return recordAttachmentLost(ctx, emitter, runID, nodeID, attachmentLoss{
			Directive: directiveAttachment,
			Path:      dir.Path,
			Reason:    lossUnreadable,
			Detail:    fmt.Sprintf("stat %s: %v", dir.Path, err),
			Required:  dir.Required,
		}, logger)
	}
	if info.IsDir() {
		return recordAttachmentLost(ctx, emitter, runID, nodeID, attachmentLoss{
			Directive: directiveAttachment,
			Path:      dir.Path,
			Reason:    lossNotAFile,
			Detail:    fmt.Sprintf("%s is a directory", dir.Path),
			Required:  dir.Required,
		}, logger)
	}
	if info.Size() > maxToolAttachmentBytes {
		return recordAttachmentLost(ctx, emitter, runID, nodeID, attachmentLoss{
			Directive: directiveAttachment,
			Path:      dir.Path,
			Reason:    lossOverCap,
			Detail: fmt.Sprintf("%s is %d bytes, over the %d cap", dir.Path,
				info.Size(), int64(maxToolAttachmentBytes)),
			Required: dir.Required,
		}, logger)
	}

	// The name is the handle the gate's descriptor will carry, so it must
	// survive the store's path sanitisation unchanged. A caller that
	// supplies nothing gets the file's base name, which is what an
	// operator would recognise in the review.
	name := sanitizeAttachmentSegment(dir.Name)
	if name == "" {
		name = sanitizeAttachmentSegment(strings.TrimSuffix(
			filepath.Base(dir.Path), filepath.Ext(dir.Path),
		))
	}
	if name == "" {
		name = fmt.Sprintf("attachment-%s", sanitizeAttachmentSegment(nodeID))
	}

	// WriteAttachment overwrites unconditionally and documents duplicate
	// handling as the caller's job. Two ways that bites here: a directive
	// can take over the name of an operator's own upload, and a loop that
	// regenerates the same deliverable can hand a gate reviewing pass N-1
	// the bytes of pass N. A STABLE name is the whole point of this verb,
	// so the collision is refused rather than suffixed away — the author
	// gets a warning naming the fix.
	if lister, ok := sink.(AttachmentLister); ok {
		if existing, listErr := lister.ListAttachments(ctx, runID); listErr == nil {
			for _, rec := range existing {
				if rec.Name == name {
					return recordAttachmentLost(ctx, emitter, runID, nodeID, attachmentLoss{
						Directive: directiveAttachment,
						Path:      dir.Path,
						Name:      name,
						Reason:    lossNameTaken,
						Detail: fmt.Sprintf(
							"%q already exists on this run (publish a revision "+
								"under its own name=)", name),
						Required: dir.Required,
					}, logger)
				}
			}
		}
	}

	f, err := os.Open(dir.Path)
	if err != nil {
		return recordAttachmentLost(ctx, emitter, runID, nodeID, attachmentLoss{
			Directive: directiveAttachment,
			Path:      dir.Path,
			Name:      name,
			Reason:    lossUnreadable,
			Detail:    fmt.Sprintf("open %s: %v", dir.Path, err),
			Required:  dir.Required,
		}, logger)
	}
	defer f.Close()

	rec := store.AttachmentRecord{
		Name:             name,
		OriginalFilename: filepath.Base(dir.Path),
		MIME:             attachmentMIME(dir),
	}
	if err := sink.WriteAttachment(ctx, runID, rec, f); err != nil {
		return recordAttachmentLost(ctx, emitter, runID, nodeID, attachmentLoss{
			Directive: directiveAttachment,
			Path:      dir.Path,
			Name:      name,
			Reason:    lossStoreWriteFailed,
			Detail:    fmt.Sprintf("write %s: %v", name, err),
			Required:  dir.Required,
		}, logger)
	}

	_, _ = emitter.AppendEvent(ctx, runID, store.Event{
		Type:   store.EventRunAttachmentPublished,
		RunID:  runID,
		NodeID: nodeID,
		Data: map[string]any{
			"attachment_name": name,
			"source":          "tool-stdout",
			"mime":            rec.MIME,
		},
	})
	logger.Logf(iterlog.LevelInfo, "📎", "Tool attachment [%s]: %s", nodeID, name)
	return nil
}
