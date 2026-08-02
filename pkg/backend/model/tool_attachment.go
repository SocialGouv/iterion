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

// AttachmentDirective is one parsed `[iterion] attachment=...` line.
// Path is host-absolute. Name defaults to the file's base name,
// sanitised; MIME is sniffed from the extension when absent.
type AttachmentDirective struct {
	Path string
	Name string
	MIME string
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
		dir := AttachmentDirective{Path: tokens[0]}
		for _, kv := range tokens[1:] {
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

// attachmentMIME resolves the stored MIME: the directive's own value
// when it is well formed, else a sniff from the extension. An unknown
// extension yields the generic binary type rather than a lie — the
// studio falls back to a download link, which is still usable.
func attachmentMIME(dir AttachmentDirective) string {
	if declared := strings.TrimSpace(dir.MIME); declared != "" {
		if parsed, _, err := mime.ParseMediaType(declared); err == nil && parsed != "" {
			return parsed
		}
	}
	if byExt := mime.TypeByExtension(filepath.Ext(dir.Path)); byExt != "" {
		if parsed, _, err := mime.ParseMediaType(byExt); err == nil && parsed != "" {
			return parsed
		}
	}
	return "application/octet-stream"
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
	f, err := os.Open(dir.Path)
	if err != nil {
		logger.Warn("Tool attachment [%s]: open %s: %v", nodeID, dir.Path, err)
		return
	}
	defer f.Close()

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
		Type:   store.EventBrowserScreenshot,
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
