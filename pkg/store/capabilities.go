package store

// Capabilities reports the optional features this backend supports.
// FilesystemRunStore exposes everything: live tail (fsnotify on
// events.jsonl), cross-process flock, PID files, and git worktree
// finalization. The cloud (Mongo) backend will turn off PIDFile and
// GitWorktree but keep LiveStream (change streams) and
// CrossProcessLock (NATS KV).
//
// See cloud-ready plan §C.1, AD-07, AD-09.
func (s *FilesystemRunStore) Capabilities() Capabilities {
	return Capabilities{
		LiveStream:       true,
		CrossProcessLock: true,
		PIDFile:          true,
		GitWorktree:      true,
	}
}
