package tools

import (
	intl "github.com/SocialGouv/claw-code-go/internal/tools"
)

// ----- pdf text extraction -----

// ExtractPDFText reads a PDF file and returns the text carried by its
// BT/ET content-stream operators. Non-text pages or encrypted PDFs
// yield an empty string rather than an error. Exported for embedders
// that expose a read tool over arbitrary workspace files — a .pdf is
// binary, so a text-lines read tool needs this to answer at all
// (internal/tools is not importable outside this module).
func ExtractPDFText(path string) (string, error) {
	return intl.ExtractText(path)
}
