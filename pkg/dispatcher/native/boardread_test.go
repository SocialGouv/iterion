package native

import "testing"

// mustBoard / mustLabels read a store's config and label vocabulary, or fail
// the test. BoardStore reports a read failure rather than substituting a
// default board or an empty vocabulary for it (see BoardStore.Board), so a
// test that ignored the error would assert against whatever the failure fell
// back to.
func mustBoard(t *testing.T, s BoardStore) *Board {
	t.Helper()
	b, err := s.Board()
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	return b
}

func mustLabels(t *testing.T, s BoardStore) []LabelUsage {
	t.Helper()
	got, err := s.AggregateLabels()
	if err != nil {
		t.Fatalf("AggregateLabels: %v", err)
	}
	return got
}
