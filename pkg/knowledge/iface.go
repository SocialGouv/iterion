package knowledge

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// MemoryStore is the backend-agnostic store for shared knowledge
// spaces. The method set is the runtime surface the memory tools
// (memory_read / memory_write / memory_list), the auto-index, and the
// pre-compact injector consume. Later phases extend this interface
// with quota accounting (UsageBytes / SetQuota), space management
// (ListSpaces / GetSpace / EnsureSpace / DeleteSpace), and export /
// import — each adapter grows to match.
//
// Implementations MUST be safe for concurrent use across runs.
type MemoryStore interface {
	// Root returns a human-meaningful display label for the space —
	// an absolute filesystem path for the FS adapter, a mem:// URI for
	// the cloud adapter. It also validates the ref, so callers can use
	// it as an early well-formedness check. Returns an error for a
	// malformed or unsupported ref.
	Root(ref SpaceRef) (string, error)

	// BuildIndex returns one IndexEntry per Markdown document in the
	// space, lexicographic by path. A missing/empty space returns an
	// empty slice without error.
	BuildIndex(ctx context.Context, ref SpaceRef) ([]IndexEntry, error)

	// Autoload returns the full content of every document matching one
	// of the relative glob patterns, deterministic (lexicographic) by
	// path. Empty patterns → empty slice. Missing documents are
	// silently skipped.
	Autoload(ctx context.Context, ref SpaceRef, patterns []string) ([]AutoloadEntry, error)

	// ListDocuments enumerates documents (not sub-spaces) directly
	// under the space-relative dir. A missing dir returns an empty
	// slice without error.
	ListDocuments(ctx context.Context, ref SpaceRef, dir string) ([]DocumentMeta, error)

	// ReadDocument returns a document's metadata + content. A missing
	// document returns an error satisfying errors.Is(err, ErrDocNotFound).
	ReadDocument(ctx context.Context, ref SpaceRef, path string) (Document, error)

	// WriteDocument creates or replaces a document and returns its new
	// metadata. Implementations enforce any space/org quota BEFORE
	// committing bytes and never partially write; an over-quota write
	// returns an error satisfying errors.Is(err, ErrQuotaExceeded) (or
	// ErrOrgQuotaExceeded for the aggregate ceiling).
	WriteDocument(ctx context.Context, ref SpaceRef, in DocumentInput) (DocumentMeta, error)

	// DeleteDocument removes a document. Deleting an absent document is
	// not an error.
	DeleteDocument(ctx context.Context, ref SpaceRef, path string) error

	// UsageBytes returns the space's current usage and its effective
	// quota (the per-space sub-cap). Used by `iterion memory du`, the
	// studio usage panel, and admin quota reporting.
	UsageBytes(ctx context.Context, ref SpaceRef) (used, quota int64, err error)
}

// Document is a memory document's metadata plus its full content.
type Document struct {
	Meta    DocumentMeta
	Content []byte
}

// DocumentMeta describes a single memory document. Path is
// space-relative, never absolute, never starts with "/". Revision is
// monotonic per document (0 until the revisioned cloud adapter lands).
// BlobKey is empty for the FS adapter and set for the cloud adapter.
type DocumentMeta struct {
	Path        string    `json:"path"`
	Title       string    `json:"title,omitempty"`
	Description string    `json:"description,omitempty"`
	Tags        []string  `json:"tags,omitempty"`
	Size        int64     `json:"size"`
	Checksum    string    `json:"checksum,omitempty"` // sha256 of content, hex
	Revision    int64     `json:"revision,omitempty"`
	UpdatedBy   string    `json:"updated_by,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
	BlobKey     string    `json:"blob_key,omitempty"`
}

// DocumentInput is the payload for WriteDocument. ExpectedRev is an
// optimistic-concurrency guard honoured by adapters that revision
// documents (0 = "no expectation"); UpdatedBy attributes the write
// (a user id, or "bot:<id>:run:<run_id>").
type DocumentInput struct {
	Path        string
	Content     []byte
	ExpectedRev int64
	UpdatedBy   string
}

// IndexEntry summarises one Markdown document for the auto-index
// system block. Mirrors the FS index shape so the cloud adapter can
// render the same prompt block.
type IndexEntry struct {
	Path        string
	Title       string
	Description string
	Tags        []string
}

// AutoloadEntry is one document's full content for the autoload
// system block / pre-compact injection.
type AutoloadEntry struct {
	Path    string
	Content []byte
}

// Sentinel errors. Callers compare with errors.Is.
var (
	// ErrDocNotFound is returned by ReadDocument for an absent document.
	ErrDocNotFound = errors.New("knowledge: document not found")
	// ErrSpaceNotFound is returned for an absent space (cloud adapter).
	ErrSpaceNotFound = errors.New("knowledge: space not found")
	// ErrUnsupportedVisibility is returned by an adapter that cannot
	// host the requested visibility (e.g. the FS adapter for tenant-
	// scoped cloud-only spaces, until the shared-tree layout lands).
	ErrUnsupportedVisibility = errors.New("knowledge: visibility not supported by this store")
)

// QuotaError is the typed over-quota failure. WriteDocument returns it
// when a write would exceed the per-space cap (Aggregate=false) or the
// per-org aggregate ceiling (Aggregate=true).
type QuotaError struct {
	Aggregate bool  // true = an aggregate ceiling, false = per-space cap
	Used      int64 // bytes currently used
	Delta     int64 // bytes the rejected write would add
	Quota     int64 // the cap that would be exceeded
	// Counter names WHICH aggregate refused, when it is not the writer's own
	// org: a global write meets the tenant-less instance ceiling
	// (InstanceUsageID), and reporting that as "org quota exceeded" sends an
	// operator to raise the wrong ceiling. Empty keeps the original wording.
	Counter string
}

func (e *QuotaError) Error() string {
	scope := "space"
	if e.Aggregate {
		scope = "org"
		if e.Counter == InstanceUsageID {
			scope = "instance"
		} else if e.Counter != "" {
			scope = "org " + e.Counter
		}
	}
	return fmt.Sprintf("knowledge: %s quota exceeded (used=%d delta=%d quota=%d)",
		scope, e.Used, e.Delta, e.Quota)
}

// ErrQuotaExceeded / ErrOrgQuotaExceeded are the errors.Is targets a
// *QuotaError matches, so callers can branch without unwrapping.
var (
	ErrQuotaExceeded    = errors.New("knowledge: quota exceeded")
	ErrOrgQuotaExceeded = errors.New("knowledge: org quota exceeded")
)

// Is lets errors.Is(err, ErrQuotaExceeded) (and ErrOrgQuotaExceeded)
// match a *QuotaError of the matching kind.
func (e *QuotaError) Is(target error) bool {
	switch target {
	case ErrQuotaExceeded:
		return !e.Aggregate
	case ErrOrgQuotaExceeded:
		return e.Aggregate
	default:
		return false
	}
}
