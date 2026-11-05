package connection

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing/fstest"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/facade"
	"github.com/SocialGouv/iterion/pkg/connector/overlay"
)

// The durable half of the connector distribution (#2757, ADR-098 decision 5):
// a package a TENANT or the PLATFORM published, held as a file map, served by
// the tier catalogs in tier_store.go. The baked floor ships in the binary; the
// two upper tiers are rows in this store.
//
// Precedence everywhere a connector id resolves: team → platform → baked —
// the bot distribution's own order (pkg/botsource), so an operator reads one
// layering rule for both. A team ships its own connector without an engine
// change; the platform tier is the deployment's override across ALL tenants
// and carries the same trust level as the image, which is why publishing to
// it is a super-admin act at the route (the route is its own lot; the store
// makes the tier REACHABLE, and nothing here grants the write).

// PlatformTenantID is the sentinel tenant the platform tier's rows live
// under: the same collection, the same validation, zero schema change. The
// ':' suffix keeps the literal outside the identity space of real team ids
// (the botsource and secrets sentinel-tenant family).
const PlatformTenantID = "platform:"

// Limits a stored package is held to. A StoredPackage is one Mongo document;
// unbounded content would silently approach the 16 MiB BSON cap and start
// failing writes with an opaque driver error. The byte cap sits an order of
// magnitude above the shipped Forgejo package (~660 KiB) and clears the
// largest measured one (GitHub, ~4.7 MiB), under the wall; the file cap keeps
// a pathological file map from turning a Get into an unbounded allocation.
// ADR-098 named this decision as P1's to make — this is it.
const (
	// MaxPackageBytes caps the sum of all file paths + contents.
	MaxPackageBytes = 12 << 20 // 12 MiB
	// MaxPackageFiles caps the file-map entry count.
	MaxPackageFiles = 2048
)

// ErrPackageNotFound reports a store miss. Distinct from ErrNotFound (the
// connection store's): a caller resolving a connector must not read "no such
// package" as "no such connection".
var ErrPackageNotFound = errors.New("connection: no such connector package")

// StoredPackage is one published connector package as a file map:
// package-relative path → content (connector.yaml, overlay.yaml, ops/*,
// triggers.yaml, the identity lock — the same documents the filesystem and
// baked tiers hold).
type StoredPackage struct {
	// TenantID owns the row: a team id, or PlatformTenantID for the platform
	// tier. Every store read takes it separately and matches it — the package
	// doc's tenant rule, applied to distribution.
	TenantID string `bson:"tenant_id" json:"tenant_id"`
	// ConnectorID is the id the package answers to — and the package's own
	// declared id, checked equal at publish: a row that answered to a
	// different name than its content declares would be reachable under one
	// and validated under the other.
	ConnectorID string `bson:"connector_id" json:"connector_id"`
	// Files maps a package-relative path to its content. Paths are validated
	// at publish (plain relative names, no escapes).
	Files map[string]string `bson:"files" json:"files"`
	// ContentHash is sha256 over the canonical (sorted path, content) stream —
	// the provenance a later pin/update story reads; recorded at publish, never
	// trusted from the caller.
	ContentHash string `bson:"content_hash" json:"content_hash"`

	// CreatedBy and CreatedAt are the FIRST publish's: a republish replaces
	// the content and UpdatedAt and keeps both (stampPublish, shared by the
	// twins).
	CreatedBy string    `bson:"created_by,omitempty" json:"created_by,omitempty"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
}

// stampPublish sets what a publish records rather than trusts, the same way
// in both twins: the content hash computed over the files, UpdatedAt now, and
// the creation's when and by whom carried from prev — the row being replaced,
// nil on a first publish (which records the caller's CreatedBy and now).
func stampPublish(pkg *StoredPackage, prev *StoredPackage, now time.Time) {
	pkg.ContentHash = hashFiles(pkg.Files)
	pkg.UpdatedAt = now
	if prev == nil {
		pkg.CreatedAt = now
		return
	}
	pkg.CreatedAt, pkg.CreatedBy = prev.CreatedAt, prev.CreatedBy
}

// hashFiles computes the content hash every write records.
func hashFiles(files map[string]string) string {
	h := sha256.New()
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write([]byte(files[p]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// validateStoredPackage is the publish gate BOTH twins run: a package is
// validated BEFORE it can serve — the git install's rule, applied to the
// durable tiers. It loads the file map through the SAME loader a run
// resolves with (overlay.LoadPackageFS over an in-memory fs — the io/fs seam
// exists precisely so no second loader is ever written), so a publish that
// would break resolution fails the write, not the next run.
func validateStoredPackage(p StoredPackage) error {
	if p.TenantID == "" {
		return errors.New("connection: a stored package names no tenant")
	}
	if err := CheckConnectorID(p.ConnectorID); err != nil {
		return err
	}
	if len(p.Files) == 0 {
		return fmt.Errorf("connection: package %q carries no files", p.ConnectorID)
	}
	if len(p.Files) > MaxPackageFiles {
		return fmt.Errorf("connection: package %q holds %d files, over the %d cap", p.ConnectorID, len(p.Files), MaxPackageFiles)
	}
	total := 0
	fsys := fstest.MapFS{}
	for path, content := range p.Files {
		total += len(path) + len(content)
		if err := checkPackagePath(path); err != nil {
			return fmt.Errorf("connection: package %q: %w", p.ConnectorID, err)
		}
		fsys[path] = &fstest.MapFile{Data: []byte(content)}
	}
	if total > MaxPackageBytes {
		return fmt.Errorf("connection: package %q is %d bytes, over the %d cap", p.ConnectorID, total, MaxPackageBytes)
	}
	pkg, err := overlay.LoadPackageFS(fsys, ".")
	if err != nil {
		return fmt.Errorf("connection: package %q does not load: %w", p.ConnectorID, err)
	}
	if pkg.Connector.ID != p.ConnectorID {
		return fmt.Errorf("connection: stored as %q but the package declares id %q — the row would answer to a name its content does not", p.ConnectorID, pkg.Connector.ID)
	}
	// The package must also build its OWN facade: two of its operations
	// mapping to one MCP tool name (`probe.issue_x.get` and
	// `probe.issue.x_get`), or one mapping to a reserved tool, load cleanly
	// and could never be served to an agent. A collision with ANOTHER
	// package depends on the composition a resolution walks, which is not
	// known here; the facade leaves such a package out when it is built.
	if _, err := facade.New(pkg); err != nil {
		return fmt.Errorf("connection: package %q cannot be served as MCP tools: %w", p.ConnectorID, err)
	}
	return nil
}

// checkPackagePath refuses a file-map path that is not a plain relative name:
// a stored package is never written to disk by iterion, but a path with `..`
// or an absolute anchor in durable content is a shape no reader may receive.
func checkPackagePath(path string) error {
	if path == "" || path == "." || path == ".." {
		return fmt.Errorf("empty or dot path")
	}
	if path[0] == '/' || path[0] == '\\' {
		return fmt.Errorf("path %q is absolute", path)
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("path %q is not a plain relative name", path)
		}
	}
	// A backslash is a separator on some filesystems and an ordinary
	// character on this one: "a\..\b" would pass the segment check above
	// while reading as an escape on Windows, so the file map refuses the
	// byte outright — the fs.FS form is slash-separated by definition.
	if strings.ContainsAny(path, `\`) {
		return fmt.Errorf("path %q is not a plain relative name (a backslash is a separator elsewhere)", path)
	}
	return nil
}

// PackageStore persists published connector packages.
//
// Every read takes the tenant as its FIRST argument — the package doc's rule,
// unchanged: a platform row and a team row differ ONLY in the tenant they are
// stored under, and a team must never read another team's package.
type PackageStore interface {
	// Publish validates and stores the package, creating or replacing the
	// (tenant, connector) row. The validation is the store's, not the
	// caller's: every writer passes it, or the rule "validated before it can
	// serve" is a convention one route can forget.
	Publish(ctx context.Context, pkg StoredPackage) error
	// Get returns one tenant's package for a connector.
	Get(ctx context.Context, tenantID, connectorID string) (StoredPackage, error)
	// Retract removes one tenant's package. ErrPackageNotFound when absent.
	Retract(ctx context.Context, tenantID, connectorID string) error
	// ListByTenant returns a tenant's packages, sorted by connector id.
	ListByTenant(ctx context.Context, tenantID string) ([]StoredPackage, error)
	// ListConnectorIDs returns the connector ids of a tenant's packages,
	// sorted, WITHOUT their files: what a tier's listing needs, at the cost
	// of the ids rather than of every package's content (up to
	// MaxPackageBytes each).
	ListConnectorIDs(ctx context.Context, tenantID string) ([]string, error)
}

// ---- in-memory twin (tests / local) ----

// MemoryPackageStore is the in-process PackageStore twin.
type MemoryPackageStore struct {
	mu    sync.RWMutex
	rows  map[string]StoredPackage
	clock func() time.Time
}

func NewMemoryPackageStore() *MemoryPackageStore {
	return &MemoryPackageStore{rows: map[string]StoredPackage{}, clock: time.Now}
}

// WithClock pins the clock, so a test can assert on timestamps.
func (s *MemoryPackageStore) WithClock(now func() time.Time) *MemoryPackageStore {
	s.clock = now
	return s
}

func packageKey(tenantID, connectorID string) string { return tenantID + "/" + connectorID }

func (s *MemoryPackageStore) Publish(_ context.Context, pkg StoredPackage) error {
	if err := validateStoredPackage(pkg); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := packageKey(pkg.TenantID, pkg.ConnectorID)
	if prev, ok := s.rows[key]; ok {
		stampPublish(&pkg, &prev, s.clock())
	} else {
		stampPublish(&pkg, nil, s.clock())
	}
	s.rows[key] = pkg
	return nil
}

func (s *MemoryPackageStore) Get(_ context.Context, tenantID, connectorID string) (StoredPackage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.rows[packageKey(tenantID, connectorID)]
	if !ok {
		return StoredPackage{}, ErrPackageNotFound
	}
	return p, nil
}

func (s *MemoryPackageStore) Retract(_ context.Context, tenantID, connectorID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := packageKey(tenantID, connectorID)
	if _, ok := s.rows[key]; !ok {
		return ErrPackageNotFound
	}
	delete(s.rows, key)
	return nil
}

func (s *MemoryPackageStore) ListByTenant(_ context.Context, tenantID string) ([]StoredPackage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []StoredPackage
	for _, p := range s.rows {
		if p.TenantID == tenantID {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConnectorID < out[j].ConnectorID })
	return out, nil
}

func (s *MemoryPackageStore) ListConnectorIDs(_ context.Context, tenantID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for _, p := range s.rows {
		if p.TenantID == tenantID {
			out = append(out, p.ConnectorID)
		}
	}
	sort.Strings(out)
	return out, nil
}
