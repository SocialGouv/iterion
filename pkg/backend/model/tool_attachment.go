package model

import (
	"context"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// toolAttachmentDirective is the line prefix a tool node uses to hand a
// file it produced to the run, so a downstream human gate can SHOW it.
//
// Shape: `[iterion] attachment=<path> [name=<n>] [mime=<type/subtype>]`
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
}

// attachmentDirectiveKeys are the trailing `key=value` tokens the
// directive accepts. Anything else belongs to the PATH — see
// splitAttachmentTail.
var attachmentDirectiveKeys = []string{"name", "mime"}

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
			}
		}
		found = append(found, dir)
	}
	return found
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

// publishToolAttachment reads the file a tool declared and persists it
// as a run attachment, so the studio can fetch it through the existing
// `/api/runs/{id}/attachments/{name}` route.
//
// Failures are logged and non-fatal: a missing or unreadable file must
// never abort a tool node. The tool's own output is the contract with
// the workflow; the directive is how the bytes reach a human.
func publishToolAttachment(
	ctx context.Context,
	sink AttachmentWriter,
	emitter EventEmitter,
	runID, nodeID string,
	dir AttachmentDirective,
	logger *iterlog.Logger,
) {
	info, err := os.Stat(dir.Path)
	if err != nil {
		logger.Warn("Tool attachment [%s]: stat %s: %v", nodeID, dir.Path, err)
		return
	}
	if info.IsDir() {
		logger.Warn("Tool attachment [%s]: %s is a directory", nodeID, dir.Path)
		return
	}
	if info.Size() > maxToolAttachmentBytes {
		logger.Warn("Tool attachment [%s]: %s is %d bytes, over the %d cap — skipped",
			nodeID, dir.Path, info.Size(), int64(maxToolAttachmentBytes))
		return
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
					logger.Warn(
						"Tool attachment [%s]: %q already exists on this run — skipped "+
							"(publish a revision under its own name=)", nodeID, name)
					return
				}
			}
		}
	}

	f, err := os.Open(dir.Path)
	if err != nil {
		logger.Warn("Tool attachment [%s]: open %s: %v", nodeID, dir.Path, err)
		return
	}
	defer f.Close()

	rec := store.AttachmentRecord{
		Name:             name,
		OriginalFilename: filepath.Base(dir.Path),
		MIME:             attachmentMIME(dir),
	}
	if err := sink.WriteAttachment(ctx, runID, rec, f); err != nil {
		logger.Warn("Tool attachment [%s]: write %s: %v", nodeID, name, err)
		return
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
}
