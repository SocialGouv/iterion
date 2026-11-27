package store

import (
	"context"
	"errors"
	"fmt"
	"time"
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
	return capability[RunListingStore](s)
}

// RunWindow is the question a run listing asks a store that can answer it
// with one indexed read (RunWindowListingStore). Empty fields mean no filter.
//
// Every predicate here is one the store evaluates on the persisted document,
// on fields both value formats store identically (plain BSON, outside the
// exact codec's paths; record_format_listing_test.go pins that). A predicate
// only Go can evaluate stays with the caller, who then must not push Limit
// down: the store would stop at Limit rows the caller then thins.
//
// The window orders and bounds runs by their created_at date: a document with
// no date in created_at is outside it. Every writer stamps one (Run.CreatedAt
// is never omitted), and the id-by-id scan could not page past such a run
// either.
type RunWindow struct {
	// Status is an exact match on the run status.
	Status RunStatus
	// Workflow is an exact match on the workflow name.
	Workflow string
	// Since keeps runs with UpdatedAt >= Since.
	Since time.Time
	// TenantIDs narrows the answer to runs of these tenants; an empty
	// member names the runs that carry no tenant. Empty = no narrowing
	// beyond the tenant scope the context imposes, which this never
	// widens.
	TenantIDs []string
	// Before is the keyset bound: only runs strictly older than it in the
	// (CreatedAt descending, ID descending) order are returned. A zero
	// Before.CreatedAt means no bound.
	Before RunKey
	// Limit caps the records returned; 0 = no cap. Undecodable documents
	// never count against it.
	Limit int
}

// RunKey is one run's position in the newest-first listing order.
type RunKey struct {
	CreatedAt time.Time
	ID        string
}

// SkippedRun is a document a window read matched but could not decode. One
// bad run never fails the page: the caller reports it per id, exactly as a
// per-id load that failed.
type SkippedRun struct {
	ID  string
	Err error
}

// RunWindowListingStore is an optional interface a store implements to answer
// a listing window in ONE read, ordered (CreatedAt descending, ID
// descending), instead of the caller listing every id and loading each.
//
// Records are the listing projection (RunListingStore): no recorded workflow
// source, SourceOmitted set. The tenant scope of ctx applies as it does to
// every other read. A deleted run is never returned; a document of an unknown
// schema version, or one that does not decode, is a SkippedRun carrying the
// refusal LoadRunForListing answers for it.
//
// Callers MUST nil-check via AsRunWindowListingStore and keep the per-id path
// for a store that does not implement it.
type RunWindowListingStore interface {
	ListRunWindow(ctx context.Context, w RunWindow) ([]*Run, []SkippedRun, error)
}

// AsRunWindowListingStore returns s as RunWindowListingStore, or nil when the
// backend has no indexed listing.
func AsRunWindowListingStore(s RunStore) RunWindowListingStore {
	return capability[RunWindowListingStore](s)
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
