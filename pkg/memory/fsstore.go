package memory

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// FSStore is the local-filesystem implementation of
// knowledge.MemoryStore. It hosts spaces under the global iterion data
// dir and reuses the path-clamped Scope primitive for all document IO,
// so the security guarantees (no "../" or absolute escapes) are shared
// with the legacy scope API.
type FSStore struct {
	root string // base data dir; empty = resolve store.GlobalIterionDataDir() lazily
}

var _ knowledge.MemoryStore = (*FSStore)(nil)

// DefaultFSStore returns an FSStore rooted at the global iterion data
// dir, resolved lazily on each use so a test's ITERION_HOME override
// (or any runtime change) is honoured.
func DefaultFSStore() *FSStore { return &FSStore{} }

func (s *FSStore) baseDir() string {
	if s.root != "" {
		return s.root
	}
	return store.GlobalIterionDataDir()
}

// LegacyBotRef builds the SpaceRef a legacy `memory: scope:` block
// resolves to: an explicit project-visibility space keyed by the
// encoded project key of memBase (the run workDir, or the repo root
// when project_root is set). The resolved on-disk path is identical
// to the pre-knowledge WorkspaceMemoryDir(memBase)/<scope> layout,
// so existing local bots see the same files without masquerading as a
// structured bot-qualified space.
func LegacyBotRef(memBase, scope string) knowledge.SpaceRef {
	return knowledge.SpaceRef{
		Visibility: knowledge.VisibilityProject,
		ProjectID:  ProjectKey(memBase),
		Name:       scope,
	}
}

// ProjectKey encodes a memory base dir (run workdir or repo root) into
// the stable, filesystem-safe project key used in the on-disk layout.
func ProjectKey(memBase string) string {
	if memBase == "" {
		return ""
	}
	abs := memBase
	if !filepath.IsAbs(abs) {
		if resolved, err := filepath.Abs(memBase); err == nil {
			abs = resolved
		}
	}
	return store.EncodeWorkDirKey(abs)
}

// SpaceRefInputs are the per-run identity values used to resolve a
// structured memory space to a concrete SpaceRef.
type SpaceRefInputs struct {
	TenantID  string // org tenant ("" → local)
	UserID    string // current operator/user
	ProjectID string // encoded project key (store.EncodeWorkDirKey)
	BotID     string // launching bot id
}

// ResolveSpaceRef builds a knowledge.SpaceRef from a DSL-declared memory
// space (visibility + name + optional bot/user overrides) and the run's
// resolved identity. Fields a visibility doesn't need stay empty (the FS
// adapter maps an empty tenant/user to "local").
func ResolveSpaceRef(vis knowledge.Visibility, name, botOverride, userOverride string, in SpaceRefInputs) knowledge.SpaceRef {
	ref := knowledge.SpaceRef{Visibility: vis, Name: name, TenantID: in.TenantID}
	switch vis {
	case knowledge.VisibilityBot:
		ref.ProjectID = in.ProjectID
		ref.BotID = cmp.Or(botOverride, in.BotID)
	case knowledge.VisibilityProject:
		ref.ProjectID = in.ProjectID
	case knowledge.VisibilityUser:
		u := in.UserID
		if userOverride != "" && userOverride != "self" {
			u = userOverride
		}
		if u == "" {
			u = "local"
		}
		ref.UserID = u
	case knowledge.VisibilityGlobal:
		ref.TenantID = "" // instance-wide, not tenant-scoped
	}
	return ref
}

// scopeFor resolves a SpaceRef to a path-clamped Scope on disk.
//
// Layout:
//   - bot → <base>/projects/<projectID>/bots/<botID>/memory/<name>
//     (structured per-bot isolation)
//   - project → <base>/projects/<projectID>/memory/<name> (also the
//     legacy `memory: scope:` path returned by LegacyBotRef).
//   - user → <base>/shared/tenants/<tenant>/users/<user>/<name>
//   - org → <base>/shared/tenants/<tenant>/org/<name>
//   - cross_project → <base>/shared/tenants/<tenant>/cross_project/<name>
//   - global → <base>/global/<name>
//
// In single-tenant local mode an empty tenant maps to "local".
func (s *FSStore) scopeFor(ref knowledge.SpaceRef) (*Scope, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	base := s.baseDir()
	var root string
	switch ref.Visibility {
	case knowledge.VisibilityBot:
		if ref.ProjectID == "" {
			return nil, fmt.Errorf("memory: empty project for space %q", ref.Name)
		}
		if ref.BotID == "" {
			return nil, fmt.Errorf("memory: empty bot for space %q", ref.Name)
		}
		root = filepath.Join(base, "projects", ref.ProjectID, "bots", ref.BotID, "memory", ref.Name)
	case knowledge.VisibilityProject:
		if ref.ProjectID == "" {
			return nil, fmt.Errorf("memory: empty project for space %q", ref.Name)
		}
		root = filepath.Join(base, "projects", ref.ProjectID, "memory", ref.Name)
	case knowledge.VisibilityUser:
		root = filepath.Join(base, "shared", "tenants", pathSeg(ref.TenantID), "users", pathSeg(ref.UserID), ref.Name)
	case knowledge.VisibilityOrg:
		root = filepath.Join(base, "shared", "tenants", pathSeg(ref.TenantID), "org", ref.Name)
	case knowledge.VisibilityCrossProject:
		root = filepath.Join(base, "shared", "tenants", pathSeg(ref.TenantID), "cross_project", ref.Name)
	case knowledge.VisibilityGlobal:
		root = filepath.Join(base, "global", ref.Name)
	default:
		// private (run-scoped, ephemeral) has no durable FS home yet.
		return nil, fmt.Errorf("%w: %q", knowledge.ErrUnsupportedVisibility, ref.Visibility)
	}
	return &Scope{root: root}, nil
}

// pathSeg makes an identity component safe for a single path segment,
// mapping empty to "local" (single-tenant / local mode).
func pathSeg(v string) string {
	if v == "" {
		return "local"
	}
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return "local"
	}
	return out
}

// Root returns the space's absolute on-disk path.
func (s *FSStore) Root(ref knowledge.SpaceRef) (string, error) {
	sc, err := s.scopeFor(ref)
	if err != nil {
		return "", err
	}
	return sc.Root(), nil
}

// BuildIndex returns one IndexEntry per Markdown document in the space.
func (s *FSStore) BuildIndex(_ context.Context, ref knowledge.SpaceRef) ([]knowledge.IndexEntry, error) {
	sc, err := s.scopeFor(ref)
	if err != nil {
		return nil, err
	}
	entries, err := sc.BuildIndex()
	if err != nil {
		return nil, err
	}
	out := make([]knowledge.IndexEntry, len(entries))
	for i, e := range entries {
		out[i] = knowledge.IndexEntry{Path: e.Path, Title: e.Title, Description: e.Description, Tags: e.Tags}
	}
	return out, nil
}

// Autoload returns the full content of documents matching the patterns.
func (s *FSStore) Autoload(_ context.Context, ref knowledge.SpaceRef, patterns []string) ([]knowledge.AutoloadEntry, error) {
	sc, err := s.scopeFor(ref)
	if err != nil {
		return nil, err
	}
	entries, err := sc.Autoload(patterns)
	if err != nil {
		return nil, err
	}
	out := make([]knowledge.AutoloadEntry, len(entries))
	for i, e := range entries {
		out[i] = knowledge.AutoloadEntry{Path: e.Path, Content: e.Content}
	}
	return out, nil
}

// ListDocuments enumerates files directly under the space-relative dir.
func (s *FSStore) ListDocuments(_ context.Context, ref knowledge.SpaceRef, dir string) ([]knowledge.DocumentMeta, error) {
	sc, err := s.scopeFor(ref)
	if err != nil {
		return nil, err
	}
	files, err := sc.List(dir)
	if err != nil {
		return nil, err
	}
	out := make([]knowledge.DocumentMeta, 0, len(files))
	for _, f := range files {
		out = append(out, knowledge.DocumentMeta{Path: f})
	}
	return out, nil
}

// ReadDocument returns a document's metadata + content. A missing
// document returns an error satisfying errors.Is(err, ErrDocNotFound).
func (s *FSStore) ReadDocument(_ context.Context, ref knowledge.SpaceRef, path string) (knowledge.Document, error) {
	if err := knowledge.ValidateDocPath(path); err != nil {
		return knowledge.Document{}, err
	}
	sc, err := s.scopeFor(ref)
	if err != nil {
		return knowledge.Document{}, err
	}
	data, err := sc.Read(path)
	if err != nil {
		if os.IsNotExist(err) {
			return knowledge.Document{}, fmt.Errorf("%w: %q", knowledge.ErrDocNotFound, path)
		}
		return knowledge.Document{}, err
	}
	return knowledge.Document{
		Meta: knowledge.DocumentMeta{
			Path:     path,
			Size:     int64(len(data)),
			Checksum: knowledge.ChecksumHex(data),
		},
		Content: data,
	}, nil
}

// WriteDocument creates or replaces a document and returns its
// metadata. It enforces the per-document size cap and both the
// per-space and org-aggregate quota ceilings BEFORE committing bytes —
// an over-quota write returns a *knowledge.QuotaError and never lands
// on disk. Counters are updated only after the write succeeds, under
// the global quota lock, so concurrent writers stay consistent.
func (s *FSStore) WriteDocument(_ context.Context, ref knowledge.SpaceRef, in knowledge.DocumentInput) (knowledge.DocumentMeta, error) {
	// The SHARED clamp, not just the scope one below. Scope.Resolve keeps a
	// path inside the space but silently normalises it through filepath.Clean,
	// so this adapter used to accept `./notes.md` and store it as `notes.md`
	// while the cloud adapter rejected the same call — one contract with two
	// behaviours, and the local one hid every path defect until it reached
	// production.
	if err := knowledge.ValidateDocPath(in.Path); err != nil {
		return knowledge.DocumentMeta{}, err
	}
	sc, err := s.scopeFor(ref)
	if err != nil {
		return knowledge.DocumentMeta{}, err
	}
	newSize := int64(len(in.Content))
	if max := maxDocumentSize(); newSize > max {
		return knowledge.DocumentMeta{}, &knowledge.QuotaError{Aggregate: false, Used: 0, Delta: newSize, Quota: max}
	}
	abs, err := sc.Resolve(in.Path)
	if err != nil {
		return knowledge.DocumentMeta{}, err
	}
	base := s.baseDir()
	lock, err := acquireQuotaLock(base)
	if err != nil {
		return knowledge.DocumentMeta{}, err
	}
	defer func() { _ = lock.Unlock() }()

	// Read the existing size INSIDE the quota lock. Two concurrent overwrites
	// that both stat before acquiring the lock would compute delta off the same
	// pre-write oldSize and double-count it into the quota counters.
	var oldSize int64
	if fi, statErr := os.Stat(abs); statErr == nil {
		oldSize = fi.Size()
	}
	delta := newSize - oldSize

	space, err := readSidecar(spaceSidecarPath(base, ref))
	if err != nil {
		return knowledge.DocumentMeta{}, err
	}
	agg, err := readSidecar(aggregatePath(base))
	if err != nil {
		return knowledge.DocumentMeta{}, err
	}
	if err := checkCeilings(ref, space, agg, delta); err != nil {
		return knowledge.DocumentMeta{}, err
	}
	if err := sc.Write(in.Path, in.Content); err != nil {
		return knowledge.DocumentMeta{}, err
	}
	if err := commitDelta(base, ref, space, agg, delta); err != nil {
		return knowledge.DocumentMeta{}, err
	}
	return knowledge.DocumentMeta{
		Path:      in.Path,
		Size:      newSize,
		Checksum:  knowledge.ChecksumHex(in.Content),
		UpdatedBy: in.UpdatedBy,
		UpdatedAt: time.Now(),
	}, nil
}

// DeleteDocument removes a document (no-op if absent) and credits its
// bytes back to the per-space and aggregate counters.
func (s *FSStore) DeleteDocument(_ context.Context, ref knowledge.SpaceRef, path string) error {
	if err := knowledge.ValidateDocPath(path); err != nil {
		return err
	}
	sc, err := s.scopeFor(ref)
	if err != nil {
		return err
	}
	abs, err := sc.Resolve(path)
	if err != nil {
		return err
	}
	fi, statErr := os.Stat(abs)
	if os.IsNotExist(statErr) {
		return nil
	}
	var size int64
	if statErr == nil {
		size = fi.Size()
	}
	base := s.baseDir()
	lock, err := acquireQuotaLock(base)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()

	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		return err
	}
	if size > 0 {
		space, err := readSidecar(spaceSidecarPath(base, ref))
		if err != nil {
			return err
		}
		agg, err := readSidecar(aggregatePath(base))
		if err != nil {
			return err
		}
		if err := commitDelta(base, ref, space, agg, -size); err != nil {
			return err
		}
	}
	return nil
}

// UsageBytes returns the space's tracked usage and its effective
// per-space quota (explicit override, else env/default for the
// visibility). Bytes written through the legacy Scope API (or before
// this adapter) are not counted — quota gates new growth, not history.
func (s *FSStore) UsageBytes(_ context.Context, ref knowledge.SpaceRef) (int64, int64, error) {
	if err := ref.Validate(); err != nil {
		return 0, 0, err
	}
	base := s.baseDir()
	space, err := readSidecar(spaceSidecarPath(base, ref))
	if err != nil {
		return 0, 0, err
	}
	return space.UsedBytes, effectiveQuota(space.QuotaBytes, spaceQuotaFor(ref.Visibility)), nil
}
