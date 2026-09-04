package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// LocalScopeTeam is the synthetic team id stamped on every secret held by
// the local (desktop / headless) file-backed store. Resolution reuses the
// cloud path verbatim — ResolveGeneric requires a non-empty team id and the
// Mongo store keys on a tenant pulled from context; the file store has no
// tenant, so it pins this constant instead, and ResolveGeneric is called as
// ResolveGeneric(ctx, store, LocalScopeTeam, "", names, sealer).
const LocalScopeTeam = "local"

// localSecretsFileVersion is the on-disk schema version, bumped only on a
// breaking change to the persisted shape (migration hook, not used yet).
const localSecretsFileVersion = 1

// LocalSecretsFileName is the basename of the sealed store, used for both
// the global (~/.iterion) and per-project (<repo>/.iterion) files.
const LocalSecretsFileName = "secrets.json"

// localSecretRecord is the persisted form of a GenericSecret. It exists
// because GenericSecret.SealedSecret carries `json:"-"` (so a sealed blob
// is never serialised out of an API response); on disk we DO need to keep
// the sealed bytes. encoding/json renders []byte as base64, so the sealed
// blob lands as a base64 string — never plaintext.
type localSecretRecord struct {
	ID           string     `json:"id"`
	ScopeUserID  string     `json:"scope_user_id,omitempty"`
	Name         string     `json:"name"`
	Last4        string     `json:"last4,omitempty"`
	Sealed       []byte     `json:"sealed"`
	CreatedBy    string     `json:"created_by,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	Fingerprint  string     `json:"fingerprint,omitempty"`
	AllowedHosts []string   `json:"allowed_hosts,omitempty"`
}

type localSecretsFile struct {
	Version int                 `json:"version"`
	Secrets []localSecretRecord `json:"secrets"`
}

func recordFromGeneric(s GenericSecret) localSecretRecord {
	return localSecretRecord{
		ID:           s.ID,
		ScopeUserID:  s.ScopeUserID,
		Name:         s.Name,
		Last4:        s.Last4,
		Sealed:       s.SealedSecret,
		CreatedBy:    s.CreatedBy,
		CreatedAt:    s.CreatedAt,
		LastUsedAt:   s.LastUsedAt,
		Fingerprint:  s.Fingerprint,
		AllowedHosts: s.AllowedHosts,
	}
}

func (r localSecretRecord) toGeneric() GenericSecret {
	return GenericSecret{
		ID:           r.ID,
		TenantID:     "",
		ScopeTeamID:  LocalScopeTeam,
		ScopeUserID:  r.ScopeUserID,
		Name:         r.Name,
		Last4:        r.Last4,
		SealedSecret: r.Sealed,
		CreatedBy:    r.CreatedBy,
		CreatedAt:    r.CreatedAt,
		LastUsedAt:   r.LastUsedAt,
		Fingerprint:  r.Fingerprint,
		AllowedHosts: r.AllowedHosts,
	}
}

// FileGenericSecretStore is a filesystem-backed GenericSecretStore for the
// local (desktop / CLI / non-cloud studio) path. Values are AES-GCM sealed
// by the caller (SealGenericSecret) before Create/Update — the file never
// holds plaintext. The store keeps an in-memory index guarded by a mutex
// and persists the whole file atomically (0600) on every mutation, mirroring
// the plugin registry's load/save idiom.
type FileGenericSecretStore struct {
	mu      sync.Mutex
	path    string
	secrets map[string]GenericSecret // keyed by ID
}

// NewFileGenericSecretStore opens (or lazily creates) a sealed secrets file
// at path. A missing file is treated as an empty store; a malformed file is
// a hard error (we never silently discard a store the operator may be able
// to recover).
func NewFileGenericSecretStore(path string) (*FileGenericSecretStore, error) {
	s := &FileGenericSecretStore{path: path, secrets: make(map[string]GenericSecret)}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *FileGenericSecretStore) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // empty store
		}
		return fmt.Errorf("secrets: read local store %s: %w", s.path, err)
	}
	if len(data) == 0 {
		return nil
	}
	var f localSecretsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("secrets: parse local store %s: %w", s.path, err)
	}
	for _, rec := range f.Secrets {
		s.secrets[rec.ID] = rec.toGeneric()
	}
	return nil
}

// persist writes the current in-memory index to disk atomically at 0600.
// Caller must hold s.mu.
func (s *FileGenericSecretStore) persist() error {
	recs := make([]localSecretRecord, 0, len(s.secrets))
	for _, sec := range s.secrets {
		recs = append(recs, recordFromGeneric(sec))
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Name < recs[j].Name })
	data, err := json.MarshalIndent(localSecretsFile{Version: localSecretsFileVersion, Secrets: recs}, "", "  ")
	if err != nil {
		return fmt.Errorf("secrets: marshal local store: %w", err)
	}
	if err := store.WriteFileAtomic(s.path, data, 0o600); err != nil {
		return fmt.Errorf("secrets: write local store %s: %w", s.path, err)
	}
	return nil
}

// reload discards the in-memory index and re-reads it from disk. Caller must
// hold s.mu (and, for a mutation, the cross-process file lock).
func (s *FileGenericSecretStore) reload() error {
	s.secrets = make(map[string]GenericSecret)
	return s.load()
}

// mutate runs a read-modify-write under both the in-process mutex AND a
// cross-process file lock, re-reading the file first so a concurrent write
// from another iterion process (e.g. `iterion secret set` while studio runs)
// is not clobbered by a stale full-file rewrite. apply mutates s.secrets and
// returns an error to abort without persisting (e.g. not-found).
func (s *FileGenericSecretStore) mutate(apply func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockStoreFile(s.path)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.reload(); err != nil {
		return err
	}
	if err := apply(); err != nil {
		return err
	}
	return s.persist()
}

// lockStoreFile acquires the cross-process lock guarding the store file,
// retrying briefly on contention (the peer's write is short). Returns an
// unlock func.
func lockStoreFile(path string) (func(), error) {
	lockPath := path + ".lock"
	var lastErr error
	for i := 0; i < 50; i++ { // ~1s total
		lock, err := store.AcquireFileLock(lockPath, "local-secrets")
		if err == nil {
			return func() { _ = lock.Unlock() }, nil
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	return nil, fmt.Errorf("secrets: another process holds the local secret store lock %s: %w", lockPath, lastErr)
}

func (s *FileGenericSecretStore) Create(_ context.Context, rec GenericSecret) error {
	if rec.ScopeTeamID == "" {
		rec.ScopeTeamID = LocalScopeTeam
	}
	return s.mutate(func() error {
		s.secrets[rec.ID] = rec
		return nil
	})
}

// UpsertByName atomically creates or rotates (by name) a secret, sealing value
// with sealer, under the cross-process lock — so a concurrent create of the
// same name from another goroutine/process cannot produce a duplicate record.
// On rotate, the egress host lock is overwritten only when applyHosts is true
// (callers preserve it otherwise, so a value rotation never silently broadens
// egress). Returns the resulting record and whether it was newly created.
func (s *FileGenericSecretStore) UpsertByName(sealer Sealer, name, value string, hosts []string, applyHosts bool) (GenericSecret, bool, error) {
	var out GenericSecret
	var created bool
	err := s.mutate(func() error {
		var rec GenericSecret
		found := false
		for _, r := range s.secrets {
			if r.Name == name {
				rec, found = r, true
				break
			}
		}
		if found {
			if applyHosts {
				rec.AllowedHosts = hosts
			}
		} else {
			created = true
			rec = GenericSecret{
				ID:           NewGenericSecretID(),
				ScopeTeamID:  LocalScopeTeam,
				Name:         name,
				CreatedAt:    time.Now().UTC(),
				AllowedHosts: hosts,
			}
		}
		if err := SealInto(sealer, &rec, value); err != nil {
			return err
		}
		s.secrets[rec.ID] = rec
		out = rec
		return nil
	})
	return out, created, err
}

func (s *FileGenericSecretStore) Get(_ context.Context, id string) (GenericSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.secrets[id]
	if !ok {
		return GenericSecret{}, ErrGenericSecretNotFound
	}
	return rec, nil
}

func (s *FileGenericSecretStore) Update(_ context.Context, rec GenericSecret) error {
	if rec.ScopeTeamID == "" {
		rec.ScopeTeamID = LocalScopeTeam
	}
	return s.mutate(func() error {
		if _, ok := s.secrets[rec.ID]; !ok {
			return ErrGenericSecretNotFound
		}
		s.secrets[rec.ID] = rec
		return nil
	})
}

func (s *FileGenericSecretStore) Delete(_ context.Context, id string) error {
	return s.mutate(func() error {
		if _, ok := s.secrets[id]; !ok {
			return ErrGenericSecretNotFound
		}
		delete(s.secrets, id)
		return nil
	})
}

// ListByTeam returns every secret in the file. userID is accepted for
// interface parity but the local store has no user scoping — a local secret
// is always "team"-wide (ScopeUserID empty). teamID must be LocalScopeTeam.
func (s *FileGenericSecretStore) ListByTeam(_ context.Context, teamID, _ string) ([]GenericSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]GenericSecret, 0, len(s.secrets))
	for _, rec := range s.secrets {
		if teamID != "" && rec.ScopeTeamID != teamID {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// ListByUser mirrors ListByTeam for the local store (no per-user scoping).
func (s *FileGenericSecretStore) ListByUser(ctx context.Context, teamID, userID string) ([]GenericSecret, error) {
	return s.ListByTeam(ctx, teamID, userID)
}

func (s *FileGenericSecretStore) MarkUsed(_ context.Context, id string, at time.Time) error {
	return s.mutate(func() error {
		rec, ok := s.secrets[id]
		if !ok {
			return ErrGenericSecretNotFound
		}
		t := at
		rec.LastUsedAt = &t
		s.secrets[id] = rec
		return nil
	})
}

// GetByName returns the secret with the given name, or ErrGenericSecretNotFound.
// Not part of GenericSecretStore — used by the CLI/handlers to implement
// upsert-by-name (a friendlier `secret set` than create-only).
func (s *FileGenericSecretStore) GetByName(name string) (GenericSecret, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.secrets {
		if rec.Name == name {
			return rec, true
		}
	}
	return GenericSecret{}, false
}

// LayeredGenericSecretStore composes an optional per-project store over a
// global store. Resolution (ListByTeam) merges both with the project layer
// winning by secret Name, giving the "project overrides global" precedence.
// Mutations by ID search project first then global; Create targets an
// explicit layer chosen by the caller (Global()/Project()).
type LayeredGenericSecretStore struct {
	global  *FileGenericSecretStore
	project *FileGenericSecretStore // nil when no project scope is active
}

// NewLayeredGenericSecretStore wraps a required global store and an optional
// project store (pass nil for none).
func NewLayeredGenericSecretStore(global, project *FileGenericSecretStore) *LayeredGenericSecretStore {
	return &LayeredGenericSecretStore{global: global, project: project}
}

// NewLocalLayeredStore builds the machine-global store at
// <globalDir>/secrets.json plus an optional per-project store at
// <projectStoreDir>/secrets.json — but only when that dir is distinct from the
// global one (else it would layer the same file on itself). The project layer
// overrides the global by name. Used by the CLI, the local studio wiring, and
// the studio project-switch rebuild.
func NewLocalLayeredStore(globalDir, projectStoreDir string) (*LayeredGenericSecretStore, error) {
	global, err := NewFileGenericSecretStore(filepath.Join(globalDir, LocalSecretsFileName))
	if err != nil {
		return nil, err
	}
	var project *FileGenericSecretStore
	if projectStoreDir != "" && absDir(projectStoreDir) != absDir(globalDir) {
		project, err = NewFileGenericSecretStore(filepath.Join(projectStoreDir, LocalSecretsFileName))
		if err != nil {
			return nil, err
		}
	}
	return NewLayeredGenericSecretStore(global, project), nil
}

// LocalStoreForProject builds the layered local store for a project store dir,
// sourcing the machine-global dir from GlobalIterionDataDir(). It is the single
// place that couples "the global layer lives under GlobalIterionDataDir" so the
// CLI, the studio wiring, the project-switch rebuild, and the dispatcher all
// agree without repeating it.
func LocalStoreForProject(projectStoreDir string) (*LayeredGenericSecretStore, error) {
	return NewLocalLayeredStore(store.GlobalIterionDataDir(), projectStoreDir)
}

// absDir returns the absolute form of p, or p unchanged when it can't be
// resolved (sufficient for the same-directory comparison above).
func absDir(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// Global returns the machine-wide store (the default write target).
func (l *LayeredGenericSecretStore) Global() *FileGenericSecretStore { return l.global }

// Project returns the per-project store and whether one is active.
func (l *LayeredGenericSecretStore) Project() (*FileGenericSecretStore, bool) {
	return l.project, l.project != nil
}

// ForScope returns the concrete store for a scope selector ("project" →
// the project store when active, else global; anything else → global).
func (l *LayeredGenericSecretStore) ForScope(scope string) *FileGenericSecretStore {
	if strings.EqualFold(strings.TrimSpace(scope), "project") && l.project != nil {
		return l.project
	}
	return l.global
}

func (l *LayeredGenericSecretStore) Create(ctx context.Context, rec GenericSecret) error {
	return l.global.Create(ctx, rec)
}

func (l *LayeredGenericSecretStore) Get(ctx context.Context, id string) (GenericSecret, error) {
	if l.project != nil {
		if rec, err := l.project.Get(ctx, id); err == nil {
			return rec, nil
		}
	}
	return l.global.Get(ctx, id)
}

func (l *LayeredGenericSecretStore) Update(ctx context.Context, rec GenericSecret) error {
	if l.project != nil {
		if err := l.project.Update(ctx, rec); err == nil {
			return nil
		} else if !errors.Is(err, ErrGenericSecretNotFound) {
			return err
		}
	}
	return l.global.Update(ctx, rec)
}

func (l *LayeredGenericSecretStore) Delete(ctx context.Context, id string) error {
	if l.project != nil {
		if err := l.project.Delete(ctx, id); err == nil {
			return nil
		} else if !errors.Is(err, ErrGenericSecretNotFound) {
			return err
		}
	}
	return l.global.Delete(ctx, id)
}

// ListByTeam merges global + project, project winning by Name, sorted by Name.
// Delegates to ListScoped (the single source of the merge/precedence rule) and
// drops the scope tag.
func (l *LayeredGenericSecretStore) ListByTeam(ctx context.Context, teamID, userID string) ([]GenericSecret, error) {
	scoped, err := l.ListScoped(ctx, teamID, userID)
	if err != nil {
		return nil, err
	}
	out := make([]GenericSecret, 0, len(scoped))
	for _, sc := range scoped {
		out = append(out, sc.Secret)
	}
	return out, nil
}

func (l *LayeredGenericSecretStore) ListByUser(ctx context.Context, teamID, userID string) ([]GenericSecret, error) {
	return l.ListByTeam(ctx, teamID, userID)
}

// ScopedSecret pairs a secret with the layer it resolved from ("global" |
// "project"), so a UI/CLI can show which layer owns each entry.
type ScopedSecret struct {
	Secret GenericSecret
	Scope  string
}

// ListScoped returns every secret tagged with its owning layer, project
// overriding global by name (a name present in both appears once, scope
// "project"). Sorted by Name.
func (l *LayeredGenericSecretStore) ListScoped(ctx context.Context, teamID, userID string) ([]ScopedSecret, error) {
	byName := make(map[string]ScopedSecret)
	globals, err := l.global.ListByTeam(ctx, teamID, userID)
	if err != nil {
		return nil, err
	}
	for _, rec := range globals {
		byName[rec.Name] = ScopedSecret{Secret: rec, Scope: "global"}
	}
	if l.project != nil {
		projects, err := l.project.ListByTeam(ctx, teamID, userID)
		if err != nil {
			return nil, err
		}
		for _, rec := range projects {
			byName[rec.Name] = ScopedSecret{Secret: rec, Scope: "project"}
		}
	}
	out := make([]ScopedSecret, 0, len(byName))
	for _, sc := range byName {
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Secret.Name < out[j].Secret.Name })
	return out, nil
}

func (l *LayeredGenericSecretStore) MarkUsed(ctx context.Context, id string, at time.Time) error {
	if l.project != nil {
		if err := l.project.MarkUsed(ctx, id, at); err == nil {
			return nil
		} else if !errors.Is(err, ErrGenericSecretNotFound) {
			return err
		}
	}
	return l.global.MarkUsed(ctx, id, at)
}

// ValidGenericSecretName reports whether name is a legal secret name: a
// non-empty identifier of [A-Za-z_][A-Za-z0-9_]* up to 128 chars (the same
// rule the cloud secret routes enforce). A valid name is also a valid POSIX
// env-var name, which the file-mount / env-indirection paths rely on.
func ValidGenericSecretName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// ValidateTokenShape rejects credential material that could not possibly
// authenticate — a bearer token or api-key value must be a single line of
// printable ASCII/UTF-8, never a terminal transcript, credentials.json paste
// or CLI banner. The check is format-agnostic on purpose (no vendor prefix
// pin, which changes over time) and covers the two paid ingestion failures:
// an accessToken with embedded newlines/ANSI escapes rendering every LLM
// call `Bearer <transcript>` → 401, and an api-key secret with a leading
// tab/space that fools string-equality auth on the server side.
//
// Empty and NUL bytes are refused too — an empty value is caught by the
// caller's own presence check but doubling up here means a future call site
// cannot introduce the regression by accident.
//
// The kind argument is used only to phrase the error message so an operator
// sees WHAT the server rejected (accessToken vs api-key secret).
func ValidateTokenShape(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s is empty", kind)
	}
	for i, r := range value {
		switch {
		case r == 0x00:
			return fmt.Errorf("%s contains a NUL byte at position %d — this looks like binary data, not a token", kind, i)
		case r == '\n' || r == '\r':
			return fmt.Errorf("%s contains a newline at position %d — this looks like a terminal transcript or credentials.json paste, not a bare token", kind, i)
		case r == '\t':
			return fmt.Errorf("%s contains a tab at position %d — strip leading/trailing whitespace before pasting", kind, i)
		case r == ' ':
			return fmt.Errorf("%s contains a space at position %d — a bearer token has none", kind, i)
		case r < 0x20:
			return fmt.Errorf("%s contains a control character (U+%04X) at position %d — this looks like a terminal transcript, not a bare token", kind, r, i)
		case r == 0x7f:
			return fmt.Errorf("%s contains a DEL byte at position %d — strip control characters before pasting", kind, i)
		}
	}
	return nil
}

// compile-time interface checks
var (
	_ GenericSecretStore = (*FileGenericSecretStore)(nil)
	_ GenericSecretStore = (*LayeredGenericSecretStore)(nil)
)
