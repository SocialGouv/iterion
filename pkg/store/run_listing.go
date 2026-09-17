package store

import (
	"context"
	"errors"
	"fmt"
)

// RunListingStore is an optional interface a store implements to hand a
// LISTING its run documents without the fields no listing reads.
//
// The listing path loads every matching run whole, one by one, and its own
// Limit truncates only after that — so the heaviest field on the document
// decides what a board projection costs. `workflow_source` /
// `workflow_sources` are that field: they hold the text of the unit the run
// executed, which real bots in this catalog push into the hundreds of
// kilobytes, and no consumer of a listing reads either (a listing becomes a
// RunHeader, which carries neither).
//
// Implementations MUST set Run.SourceOmitted on every record they return, so
// the empty source keeps one meaning.
//
// Callers MUST nil-check via AsRunListingStore: a store that does not
// implement it is served by the whole-document path, correctly and more
// expensively.
type RunListingStore interface {
	// LoadRunForListing is LoadRun without the recorded workflow source.
	LoadRunForListing(ctx context.Context, id string) (*Run, error)
}

// AsRunListingStore returns s as RunListingStore, or nil when the backend
// cannot project.
func AsRunListingStore(s RunStore) RunListingStore {
	if s == nil {
		return nil
	}
	l, _ := s.(RunListingStore)
	return l
}

// ErrRunProjected refuses a whole-document write of a record that came from a
// listing. Such a record's recorded source was never loaded, and every SaveRun
// here REPLACES the document: saving one would drop the text of the unit the
// run executed, permanently, with no error and no trace — the run would simply
// stop being auto-rewindable.
//
// This refusal is what gives Run.SourceOmitted a reader, and therefore a
// purpose. A caller meaning to change one field of a listed run uses a granular
// setter, or re-reads the run whole first.
var ErrRunProjected = errors.New("store: refusing to save a run loaded for a listing — its recorded workflow source was never read, and this write would drop it (re-read with LoadRun, or use a granular setter)")

// GuardProjectedWrite is the refusal every whole-document SaveRun crosses.
func GuardProjectedWrite(r *Run) error {
	if r != nil && r.SourceOmitted {
		return fmt.Errorf("%w: run %s", ErrRunProjected, r.ID)
	}
	return nil
}

// stripRecordedSource blanks the recorded source on r and marks it, for a
// backend that can only project AFTER decoding. It buys the retention, not
// the decode: a listing holds every record it returns, so the megabytes that
// matter are the ones still reachable when it returns.
func stripRecordedSource(r *Run) *Run {
	if r == nil {
		return nil
	}
	r.WorkflowSource = ""
	r.WorkflowSources = nil
	r.SourceOmitted = true
	return r
}
