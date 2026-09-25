package store

// AnswerDescriptor is the map a workflow reads off a `file` answer at a human
// gate: the promoted attachment's run-unique name and its metadata. Both routes
// that answer a gate with a file — the studio upload and the CLI's
// `--answer key=@path` — project the attachment through it, so a workflow reads
// the same keys whichever route answered. The runtime adds `path` once it knows
// where the run can read the bytes (pkg/runtime/attachment_path.go).
func (r AttachmentRecord) AnswerDescriptor() map[string]any {
	return map[string]any{
		"attachment": r.Name,
		"filename":   r.OriginalFilename,
		"mime":       r.MIME,
		"size":       r.Size,
		"sha256":     r.SHA256,
	}
}
