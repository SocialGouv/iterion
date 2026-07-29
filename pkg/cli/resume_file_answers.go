package cli

import (
	"context"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// fileAnswerPrefix marks a --answer value as a path to attach rather
// than a literal string: `--answer music=@./theme.mp3`.
//
// The convention is curl's, and it applies ONLY to answers the paused
// node declares as `file`-typed (see fileAnswerFields) — every other
// answer passes through verbatim, '@' or not. Within a file field, a
// literal leading '@' is escaped as '@@'.
const fileAnswerPrefix = "@"

// fileAnswerFields returns the names of the paused human node's
// `file`-typed output fields — the ONLY answers the '@path' convention
// applies to.
//
// Scoping matters: --answer / --answers-file is a machine-fed interface
// (CI, scripts), and answers legitimately start with '@' — a chat
// mention, an npm scope, a `@v1.2` ref. Rewriting those into a file open
// would break callers that never opted into anything, so the schema, not
// the string's first byte, decides.
func fileAnswerFields(wf *ir.Workflow, nodeID string) map[string]bool {
	if wf == nil || nodeID == "" {
		return nil
	}
	node, ok := wf.Nodes[nodeID]
	if !ok {
		return nil
	}
	schema := wf.Schemas[ir.NodeOutputSchema(node)]
	if schema == nil {
		return nil
	}
	fields := make(map[string]bool)
	for _, f := range schema.Fields {
		if f.Type == ir.FieldTypeFile {
			fields[f.Name] = true
		}
	}
	return fields
}

// resolveFileAnswerFlags turns `key=@path` answers into run attachments,
// giving the CLI the same gate-upload capability the studio has.
//
// Why this exists at all: without it, uploading a file at a human gate
// would be a studio-only feature, and every headless caller — CI, a
// script, `iterion resume` in a terminal — would be locked out of any
// workflow that declares a `file` field. A capability only reachable
// through a GUI quietly makes the workflows that use it un-automatable.
//
// The CLI drives the engine in-process, so there is no staging area to
// promote from (that is an HTTP-layer concern): the bytes go straight
// into the run's attachments, which is where the promotion path lands
// them too. Both routes therefore converge on the same descriptor, and
// the engine fills in `path` sandbox-aware afterwards.
//
// Only fileFields keys are considered; everything else is returned
// untouched, as is any file-field value not prefixed with '@'. A literal
// leading '@' is written '@@'.
func resolveFileAnswerFlags(
	ctx context.Context,
	s store.RunStore,
	runID, nodeID string,
	fileFields map[string]bool,
	answers map[string]any,
) error {
	if s == nil || len(answers) == 0 || len(fileFields) == 0 {
		return nil
	}

	// Names already on the run: re-answering a gate must not clobber an
	// attachment an earlier pass produced. A failed listing is not
	// evidence of an empty run — treating it as one silently disables the
	// collision guard.
	existing, err := s.ListAttachments(ctx, runID)
	if err != nil {
		return fmt.Errorf("list existing attachments for run %s: %w", runID, err)
	}
	taken := make(map[string]bool, len(existing))
	for _, a := range existing {
		taken[a.Name] = true
	}

	for key, val := range answers {
		if !fileFields[key] {
			continue
		}
		raw, isString := val.(string)
		if !isString || !strings.HasPrefix(raw, fileAnswerPrefix) {
			continue
		}
		// '@@foo' is an escaped literal '@foo'.
		if strings.HasPrefix(raw, fileAnswerPrefix+fileAnswerPrefix) {
			answers[key] = raw[1:]
			continue
		}
		path := strings.TrimPrefix(raw, fileAnswerPrefix)
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("--answer %s=@: no file path after '@'", key)
		}
		desc, err := attachAnswerFile(ctx, s, runID, nodeID, key, path, taken)
		if err != nil {
			return err
		}
		answers[key] = desc
	}
	return nil
}

// attachAnswerFile copies one local file into the run's attachments and
// returns the descriptor the workflow reads.
func attachAnswerFile(
	ctx context.Context,
	s store.RunStore,
	runID, nodeID, field, path string,
	taken map[string]bool,
) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("--answer %s: open %s: %w", field, path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("--answer %s: stat %s: %w", field, path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("--answer %s: %s is a directory", field, path)
	}

	name := cliAttachmentName(nodeID, field, taken)
	taken[name] = true

	filename := filepath.Base(path)
	rec := store.AttachmentRecord{
		Name:             name,
		OriginalFilename: filename,
		// Extension-derived, unlike the HTTP path which sniffs content.
		// The CLI caller chose the file deliberately; there is no
		// hostile uploader to defend against here, and WriteAttachment
		// still recomputes size + sha256 from the bytes.
		MIME: mime.TypeByExtension(filepath.Ext(filename)),
	}
	if rec.MIME == "" {
		rec.MIME = "application/octet-stream"
	}
	if err := s.WriteAttachment(ctx, runID, rec, f); err != nil {
		return nil, fmt.Errorf("--answer %s: attach %s: %w", field, path, err)
	}

	// Read back so the descriptor carries the canonical size/sha256
	// WriteAttachment computed from the bytes it actually stored.
	written := rec
	if list, err := s.ListAttachments(ctx, runID); err == nil {
		for _, a := range list {
			if a.Name == name {
				written = a
				break
			}
		}
	}
	return map[string]any{
		"attachment": written.Name,
		"filename":   written.OriginalFilename,
		"mime":       written.MIME,
		"size":       written.Size,
		"sha256":     written.SHA256,
	}, nil
}

// cliAttachmentName mirrors the HTTP layer's naming so a gate answered
// from the CLI and one answered from the studio produce the same
// `{{attachments.<node>.<field>}}` reference.
func cliAttachmentName(nodeID, field string, taken map[string]bool) string {
	sanitize := func(s string) string {
		s = strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				return r
			case r == '-', r == '_', r == '.':
				return r
			}
			return '-'
		}, s)
		return strings.Trim(s, "-.")
	}
	base := sanitize(field)
	if n := sanitize(nodeID); n != "" && base != "" {
		base = n + "." + base
	} else if base == "" {
		base = n
	}
	if base == "" {
		base = "upload"
	}
	name := base
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}
