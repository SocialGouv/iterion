package connection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// LocalTenant is the tenant a CLI or desktop connection belongs to.
//
// One machine is one tenant, and the value matches `secrets.LocalScopeTeam`
// so the two local stores agree about whose they are. It exists as a constant
// rather than an empty string because empty is what an UNSET tenant looks
// like, and the resolver refuses that on purpose — a local store must be
// deliberately local, not accidentally global.
const LocalTenant = "local"

// fileStoreVersion is the on-disk format version. Read before anything else,
// so a file written by a newer iterion is refused by name instead of being
// half-understood by a decoder that silently drops what it does not know.
const fileStoreVersion = 1

// FileStore is the local Store: `~/.iterion/connections.json`, with a
// per-project override, sealed credentials inside.
//
// Its cloud twin is the Mongo implementation. Shipping this one alone would
// be a cloud hole rather than a limitation, so both sit behind the same
// interface and the same tests from the start.
type FileStore struct {
	path string

	mu    sync.Mutex
	byID  map[string]Connection
	clock func() time.Time
}

// fileEnvelope is what lands on disk.
type fileEnvelope struct {
	Version     int          `json:"version"`
	Connections []fileRecord `json:"connections"`
}

// fileRecord is the on-disk shape, deliberately NOT the domain struct.
//
// Connection.SealedPayload is `json:"-"` because that struct reaches the
// studio, and a credential — sealed or not — has no business in an API
// response. Serialising the domain type here therefore wrote every connection
// with its credential silently dropped: the record persisted, the token did
// not, and the failure surfaced much later as a connection that exists and
// cannot authenticate.
//
// A separate record is the fix and the convention (`secrets.localSecretRecord`
// does the same): the transport shape and the storage shape answer to
// different readers, so they are different types.
type fileRecord struct {
	Connection
	// Sealed re-exposes the credential for STORAGE only, under a name the
	// API shape does not have.
	Sealed []byte `json:"sealed_payload,omitempty"`
}

func toRecord(c Connection) fileRecord {
	return fileRecord{Connection: c, Sealed: c.SealedPayload}
}

func (r fileRecord) toConnection() Connection {
	c := r.Connection
	c.SealedPayload = r.Sealed
	return c
}

// NewFileStore opens (or lazily creates) a connection store at path. A missing
// file is an empty store; a malformed one is a hard error, since silently
// discarding a store an operator may be able to recover is worse than
// refusing to start.
func NewFileStore(path string) (*FileStore, error) {
	s := &FileStore{path: path, byID: map[string]Connection{}, clock: time.Now}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// DefaultPath is where a store dir keeps its connections.
func DefaultPath(storeDir string) string {
	return filepath.Join(storeDir, "connections.json")
}

func (s *FileStore) reload() error {
	s.byID = map[string]Connection{}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("connection: read %s: %w", s.path, err)
	}
	if len(data) == 0 {
		return nil
	}
	var env fileEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("connection: parse %s: %w", s.path, err)
	}
	// Checked BEFORE trusting the records: a decoder ignores fields it does
	// not know, so a v2 file would load as a v1 one with its new guarantees
	// silently absent — the failure would surface as a connection that works
	// but not as its author meant.
	if env.Version > fileStoreVersion {
		return fmt.Errorf("connection: %s was written by a newer iterion (format v%d, this build understands v%d)",
			s.path, env.Version, fileStoreVersion)
	}
	for _, rec := range env.Connections {
		s.byID[rec.ID] = rec.toConnection()
	}
	return nil
}

// persist writes the index atomically at 0600. The file holds SEALED
// credentials, so the mode is not cosmetic. Caller holds s.mu and the lock.
func (s *FileStore) persist() error {
	out := make([]fileRecord, 0, len(s.byID))
	for _, c := range s.byID {
		out = append(out, toRecord(c))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	data, err := json.MarshalIndent(fileEnvelope{Version: fileStoreVersion, Connections: out}, "", "  ")
	if err != nil {
		return fmt.Errorf("connection: marshal store: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("connection: create store dir: %w", err)
	}
	if err := store.WriteFileAtomic(s.path, data, 0o600); err != nil {
		return fmt.Errorf("connection: write %s: %w", s.path, err)
	}
	return nil
}

// mutate applies a change under the cross-process file lock, RE-READING first.
//
// The re-read is the part that matters: two iterion processes legitimately
// share this file (`iterion connections add` while a studio runs), and a
// full-file rewrite from a stale in-memory copy silently deletes whatever the
// peer wrote in between.
func (s *FileStore) mutate(apply func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lock()
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

// read serves a read from a FRESH copy of the file, so a resolution never
// hands back a credential a peer process has already revoked.
//
// It takes the in-process mutex ONLY — deliberately, and this is the contract
// the Mongo twin's conformance suite should hold it to. A reader does not need
// the cross-process lock because `mutate` writes through
// `store.WriteFileAtomic`: the file is replaced by a rename, so a concurrent
// reader observes either the whole old file or the whole new one and never a
// torn one. Taking the flock here would buy no consistency and would put its
// acquisition — with a retry loop up to a second long — in front of every
// credential resolution, which is every action call of every run.
//
// What that does NOT promise is a read-modify-write: anything that reads here
// and writes later must go through `mutate`, which re-reads under the lock.
func (s *FileStore) read(get func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return err
	}
	return get()
}

// lock takes the cross-process lock guarding the file. The primitive is
// pkg/store's, shared with the local secret store; only the short retry is
// local to each.
func (s *FileStore) lock() (func(), error) {
	lockPath := s.path + ".lock"
	var lastErr error
	for i := 0; i < 50; i++ { // ~1s
		l, err := store.AcquireFileLock(lockPath, "connections")
		if err == nil {
			return func() { _ = l.Unlock() }, nil
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	return nil, fmt.Errorf("connection: another process holds %s: %w", lockPath, lastErr)
}

func (s *FileStore) Create(_ context.Context, c Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return s.mutate(func() error {
		if _, exists := s.byID[c.ID]; exists {
			return ErrExists
		}
		if err := checkAliasFree(s.byID, c, ""); err != nil {
			return err
		}
		now := s.clock()
		c.CreatedAt, c.UpdatedAt = now, now
		s.byID[c.ID] = c
		return nil
	})
}

func (s *FileStore) Get(_ context.Context, tenantID, id string) (Connection, error) {
	var out Connection
	err := s.read(func() error {
		c, ok := s.byID[id]
		if !ok || c.TenantID != tenantID {
			return ErrNotFound
		}
		out = c
		return nil
	})
	return out, err
}

func (s *FileStore) ByAlias(_ context.Context, tenantID, connector, alias string) (Connection, error) {
	var out Connection
	err := s.read(func() error {
		for _, c := range s.byID {
			if c.TenantID == tenantID && c.Connector == connector && aliasEq(c.Alias, alias) {
				out = c
				return nil
			}
		}
		return ErrNotFound
	})
	return out, err
}

func (s *FileStore) List(_ context.Context, tenantID, connector string) ([]Connection, error) {
	var out []Connection
	err := s.read(func() error {
		for _, c := range s.byID {
			if c.TenantID != tenantID {
				continue
			}
			if connector != "" && c.Connector != connector {
				continue
			}
			out = append(out, c)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].CreatedAt.Equal(out[j].CreatedAt) {
				return out[i].ID < out[j].ID
			}
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		})
		return nil
	})
	return out, err
}

func (s *FileStore) Update(_ context.Context, c Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return s.mutate(func() error {
		prev, ok := s.byID[c.ID]
		if !ok || prev.TenantID != c.TenantID {
			return ErrNotFound
		}
		if err := checkAliasFree(s.byID, c, c.ID); err != nil {
			return err
		}
		c.SealedPayload = keptCredential(prev, c)
		c.CreatedAt = prev.CreatedAt
		c.UpdatedAt = s.clock()
		s.byID[c.ID] = c
		return nil
	})
}

func (s *FileStore) Delete(_ context.Context, tenantID, id string) error {
	return s.mutate(func() error {
		c, ok := s.byID[id]
		if !ok || c.TenantID != tenantID {
			return ErrNotFound
		}
		delete(s.byID, id)
		return nil
	})
}
