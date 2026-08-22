package worktreepool

import "sort"

// Scan classifies every worktree one store parks, oldest first.
//
// The order is the whole contract for callers that stop early: the head
// of the list is what an operator is least likely to want back, and it is
// what the pool bound evicts first.
func Scan(storeDir string, opts ScanOptions) ([]Entry, error) {
	found, err := scanStore(storeDir, opts, opts.now())
	if err != nil {
		return nil, err
	}
	sort.Slice(found, func(i, j int) bool { return found[i].ModTime.Before(found[j].ModTime) })
	ApplyKeepLast(found, opts.KeepLast)
	return found, nil
}

// SortOldestFirst orders entries gathered from several stores. Scan
// already returns one store's entries in this order; a caller that merges
// stores has to restore it across the join.
func SortOldestFirst(all []Entry) {
	sort.SliceStable(all, func(i, j int) bool { return all[i].ModTime.Before(all[j].ModTime) })
}
