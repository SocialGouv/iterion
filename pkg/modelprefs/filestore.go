package modelprefs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// FileStore is the local/desktop Store: one small JSON file under the run
// store directory. A model choice is operator convenience, not run state, so
// it lives next to the store rather than inside a run — and a corrupt or
// unreadable file degrades to "no preference recorded" instead of failing the
// surface that asked.
//
// Local mode has a single operator, so tenant and user IDs are usually empty;
// they are still part of the row key so the file format does not have to
// change if a local server ever grows a second identity.
type FileStore struct {
	path string
	mu   sync.Mutex
	// logger, when set, reports a file that could not be parsed. The degrade
	// itself is deliberate, but it is not free: the next Set rewrites the file
	// from the rows it managed to read, so an unparseable file quietly loses
	// whatever it held. A warn is the difference between that and a mystery.
	logger *iterlog.Logger
}

type loadResult struct {
	rows    map[string]Pref
	corrupt []byte
}

// SetLogger attaches a logger used to report a corrupt preferences file before
// degrading to "no preference recorded". Optional — mirrors native.Store.
func (f *FileStore) SetLogger(l *iterlog.Logger) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logger = l
}

// fileRow is the on-disk shape. The composite key is flattened into the row so
// the file reads as a plain list.
type fileRow struct {
	TenantID string `json:"tenant_id,omitempty"`
	UserID   string `json:"user_id,omitempty"`
	Pref
}

type fileDoc struct {
	Version int       `json:"version"`
	Prefs   []fileRow `json:"prefs"`
}

const fileVersion = 1

// NewFileStore returns a Store backed by <dir>/model-prefs.json. The directory
// is created on first write, not here, so constructing one is free on a
// read-only path.
func NewFileStore(dir string) *FileStore {
	return &FileStore{path: filepath.Join(dir, "model-prefs.json")}
}

func (f *FileStore) load() (loadResult, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return loadResult{rows: map[string]Pref{}}, nil
		}
		return loadResult{}, fmt.Errorf("modelprefs: read %s: %w", f.path, err)
	}
	var doc fileDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		// A hand-edited or truncated file must not brick the assistant: the
		// worst honest outcome is that the operator re-picks their model. Say
		// so, though — the next Set rewrites the file from what loaded, so
		// silence here turns a corrupt file into vanished preferences.
		if f.logger != nil {
			f.logger.Warn("modelprefs: %s is unreadable (%v); ignoring the recorded preferences — the next write will preserve it as %s before repair", f.path, err, f.corruptBackupPath())
		}
		return loadResult{rows: map[string]Pref{}, corrupt: data}, nil
	}
	out := make(map[string]Pref, len(doc.Prefs))
	for _, r := range doc.Prefs {
		p := r.Pref
		p.TenantID, p.UserID = r.TenantID, r.UserID
		out[rowKey(r.TenantID, r.UserID, p.Key)] = p
	}
	return loadResult{rows: out}, nil
}

func (f *FileStore) corruptBackupPath() string { return f.path + ".corrupt.bak" }

// preserveCorrupt writes one deterministic backup generation BEFORE the
// repairing write. Overwriting that side file is deliberate: corruption can
// be read repeatedly without minting unbounded timestamped backups, while the
// bytes about to be replaced are always the bytes the backup contains.
func (f *FileStore) preserveCorrupt(data []byte) error {
	backup := f.corruptBackupPath()
	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		return fmt.Errorf("modelprefs: create corrupt-backup dir: %w", err)
	}
	if err := store.WriteFileAtomic(backup, data, 0o600); err != nil {
		return fmt.Errorf("modelprefs: preserve corrupt preferences at %s: %w", backup, err)
	}
	if f.logger != nil {
		f.logger.Warn("modelprefs: preserved unreadable preferences at %s before repairing %s", backup, f.path)
	}
	return nil
}

func (f *FileStore) save(rows map[string]Pref) error {
	doc := fileDoc{Version: fileVersion, Prefs: make([]fileRow, 0, len(rows))}
	for _, p := range rows {
		doc.Prefs = append(doc.Prefs, fileRow{TenantID: p.TenantID, UserID: p.UserID, Pref: p})
	}
	// Stable order so the file does not churn in a diff / backup.
	sort.Slice(doc.Prefs, func(i, j int) bool {
		a, b := doc.Prefs[i], doc.Prefs[j]
		if a.TenantID != b.TenantID {
			return a.TenantID < b.TenantID
		}
		if a.UserID != b.UserID {
			return a.UserID < b.UserID
		}
		return a.Key < b.Key
	})
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("modelprefs: encode: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return fmt.Errorf("modelprefs: create store dir: %w", err)
	}
	return store.WriteFileAtomic(f.path, data, 0o644)
}

func (f *FileStore) Get(_ context.Context, tenantID, userID, key string) (*Pref, error) {
	k, err := NormalizeKey(key)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	loaded, err := f.load()
	if err != nil {
		return nil, err
	}
	if p, ok := loaded.rows[rowKey(tenantID, userID, k)]; ok {
		cp := p
		return &cp, nil
	}
	return nil, nil
}

func (f *FileStore) Set(_ context.Context, p *Pref) error {
	k, err := NormalizeKey(p.Key)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	loaded, err := f.load()
	if err != nil {
		return err
	}
	if scopeAtLimit(loaded.rows, p.TenantID, p.UserID, k) {
		return fmt.Errorf("%w: maximum %d keys per tenant/user", ErrTooManyPreferences, MaxPreferencesPerScope)
	}
	if loaded.corrupt != nil {
		if err := f.preserveCorrupt(loaded.corrupt); err != nil {
			return err
		}
	}
	row := *p
	row.Key = k
	row.UpdatedAt = nowUTC()
	loaded.rows[rowKey(p.TenantID, p.UserID, k)] = row
	return f.save(loaded.rows)
}

func (f *FileStore) Delete(_ context.Context, tenantID, userID, key string) error {
	k, err := NormalizeKey(key)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	loaded, err := f.load()
	if err != nil {
		return err
	}
	if loaded.corrupt != nil {
		if err := f.preserveCorrupt(loaded.corrupt); err != nil {
			return err
		}
	}
	delete(loaded.rows, rowKey(tenantID, userID, k))
	return f.save(loaded.rows)
}
