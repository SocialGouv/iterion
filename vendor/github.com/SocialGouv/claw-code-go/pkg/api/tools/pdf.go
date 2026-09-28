package tools

import (
	intl "github.com/SocialGouv/claw-code-go/internal/tools"
)

// ErrBudgetExceeded is returned when the extraction blows the caller's
// budget: a single FlateDecode stream inflating past it (a compressed
// bomb), or the document-wide sum of streams doing the same through
// many small ones.
var ErrBudgetExceeded = intl.ErrBudgetExceeded

// ExtractPDFText reads a PDF file and returns the text carried by its
// BT/ET content-stream operators. Non-text pages or encrypted PDFs
// yield an empty string rather than an error. maxDecompressed is the
// DOCUMENT budget: zlib holds ~1000:1 (an unbounded read turned a
// 1 MiB file into a 1 GiB slice), and many small streams sum to the
// same allocation a single bomb would make — any stream inflating past
// the budget, or the accumulated text doing so, fails the extraction
// with ErrBudgetExceeded instead of allocating. Exported for embedders
// that expose a read tool over arbitrary workspace files — a .pdf is
// binary, so a text-lines read tool needs this to answer at all
// (internal/tools is not importable outside this module).
func ExtractPDFText(path string, maxDecompressed int64) (string, error) {
	return intl.ExtractText(path, maxDecompressed)
}

// ExtractPDFTextFromBytes is ExtractPDFText over bytes the caller has
// already read and size-bounded — the single-fd form: the caller's own
// safe open, stat and read stay the only access to the file.
func ExtractPDFTextFromBytes(data []byte, maxDecompressed int64) (string, error) {
	return intl.ExtractTextFromBytes(data, maxDecompressed)
}
