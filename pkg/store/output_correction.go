package store

import "context"

// OutputCorrectionStore is the granular persistence capability for the
// bounded output-correction ledger. It exists separately from RunStore so a
// correction call cannot whole-document-replace a newer cancel, checkpoint or
// steering update owned by another authority.
type OutputCorrectionStore interface {
	SetRunOutputCorrection(ctx context.Context, runID, ledgerKey string, episode OutputCorrectionEpisode) error
}

func AsOutputCorrectionStore(s RunStore) OutputCorrectionStore {
	if s == nil {
		return nil
	}
	c, _ := s.(OutputCorrectionStore)
	return c
}
