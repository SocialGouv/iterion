package native

// setIndexLocked and dropIndexLocked are the ONLY two writes to the index
// (index_write_sweep_test.go refuses any other). Every mutation — an
// in-process mutator, the watcher applying an out-of-process event — is
// marked dirty while a rebuild's scan is in flight, so the swap that
// follows takes the index's own value for that id and never reverts what
// landed after the scan read the directory. Caller holds mu.
func (s *Store) setIndexLocked(id string, iss *Issue) {
	s.index[id] = iss
	s.markDirtyLocked(id)
}

// dropIndexLocked removes id from the index; see setIndexLocked.
func (s *Store) dropIndexLocked(id string) {
	delete(s.index, id)
	s.markDirtyLocked(id)
}
