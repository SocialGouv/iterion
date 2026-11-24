package store

import (
	"fmt"
	"path/filepath"
	"time"
)

// acquireRunWriteLock serializes run-document writes with creation, deletion
// and tombstone pruning across handles and processes. Its inode lives outside
// the purged run tree. Never garbage-collect these files with live writers:
// unlinking a held flock creates two lock domains. Callers may hold s.mu; code
// under this lock must not acquire s.mu or a second run lock. Older binaries
// using runs/<id>/.write.lock must stop before writers switch to this layout.
func (s *FilesystemRunStore) acquireRunWriteLock(id string) (RunLock, error) {
	if err := sanitizePathComponent("run ID", id); err != nil {
		return nil, err
	}
	return acquireFileLockRetry(filepath.Join(s.root, ".run-write-locks", id), fmt.Sprintf("run %s document mutation", id), 10*time.Second)
}
