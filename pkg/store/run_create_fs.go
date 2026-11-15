package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

var _ RunValueFormatCreator = (*FilesystemRunStore)(nil)

func (s *FilesystemRunStore) CreateRunWithValueFormat(ctx context.Context, spec RunCreateSpec) (*Run, error) {
	r, err := s.newRunFromSpec(ctx, spec)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.writeRunNewContext(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

// newRunFromSpec resolves canonical birth metadata against this store's real
// parent namespace. Staged publication uses it without constructing a second
// store rooted at its private staging directory.
func (s *FilesystemRunStore) newRunFromSpec(ctx context.Context, spec RunCreateSpec) (*Run, error) {
	spec, err := spec.Normalize(ctx)
	if err != nil {
		return nil, err
	}
	if err = sanitizePathComponent("run ID", spec.ID); err != nil {
		return nil, err
	}
	if spec.Status == "" {
		spec.Status = RunStatusRunning
	}
	var parent *Run
	if spec.ParentRunID != "" {
		parent, err = s.LoadRun(ctx, spec.ParentRunID)
		if err != nil {
			return nil, fmt.Errorf("%w: child run %s names parent %q: %w", ErrChildParentUnreadable, spec.ID, spec.ParentRunID, err)
		}
	}
	now := time.Now().UTC()
	r := &Run{ID: spec.ID, WorkflowName: spec.WorkflowName, ParentRunID: spec.ParentRunID, FilePath: spec.FilePath, BotID: spec.BotID, Inputs: spec.Inputs, Status: spec.Status, FormatVersion: int(spec.Format), DataProtocol: spec.DataProtocol, CreatedAt: now, UpdatedAt: now, LaunchEnv: CaptureLaunchEnv(), IterionVersion: appinfo.FullVersion()}
	ChildTrustFromParent(r, parent)
	if spec.Status == RunStatusQueued {
		r.QueuedAt = &now
	}
	return r, nil
}

// Exact births cannot reinterpret legacy payloads written before run.json.
// Logs, PID/control files and empty directories carry no exact-value records.
func (s *FilesystemRunStore) checkExactBirthNamespace(id string) error {
	if _, err := os.Stat(s.runJSONPath(id)); err == nil {
		return fmt.Errorf("store: run %s already exists: %w", id, fs.ErrExist)
	} else if !os.IsNotExist(err) {
		return err
	}
	eventPath := filepath.Join(s.runDir(id), "events.jsonl")
	if info, err := os.Stat(eventPath); err == nil {
		if info.Size() != 0 {
			return fmt.Errorf("%w: preexisting events", ErrInvalidValueFormat)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, name := range []string{"artifacts", "interactions"} {
		root := filepath.Join(s.runDir(id), name)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if os.IsNotExist(err) && path == root {
				return nil
			}
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if name == "interactions" && path == filepath.Join(root, ".answer.lock") {
				return nil
			}
			return fmt.Errorf("%w: preexisting %s payload", ErrInvalidValueFormat, name)
		})
		if err != nil {
			return err
		}
	}
	return nil
}
func marshalRunValueJSON(ctx context.Context, r *Run) ([]byte, error) {
	if err := RequireRunDataStorage(r.DataProtocol); err != nil {
		return nil, err
	}
	raw, err := EncodeValueRecordJSON(ctx, ValueFormat(r.FormatVersion), ExactRun, r)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err = json.Indent(&out, raw, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), ctx.Err()
}
