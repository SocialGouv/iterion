package runview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// LegacyBareDigestMatches reports whether run r recorded the BARE digest of
// bundle b's main.bot — the digest of the source bytes alone, which every
// surface that compiled a bundle's main.bot without promoting it recorded
// (the studio's file picker, the dispatcher's service path, the trigger
// launcher, rewind, the export) before the promotion reached them. Since
// then the digest folds the bundle's prompts/*.md and presets/*.md in, so
// such a run, resumed, compares a different digest against the one it
// recorded and is refused as a source change that never happened. This is
// the narrow class where the two differ: a bundle that ships prompts/ or
// presets/ (six of the shipped bots, whats-next among them, which parks on
// human gates for hours).
func LegacyBareDigestMatches(r *store.Run, b *bundle.Bundle) bool {
	if r == nil || b == nil || r.WorkflowHash == "" || b.IterPath == "" {
		return false
	}
	src, err := os.ReadFile(b.IterPath)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(src)
	return hex.EncodeToString(sum[:]) == r.WorkflowHash
}

// MigrateLegacyBareDigest rewrites, once and in place, the digest a run
// recorded before its bundle's prompts entered the workflow digest, to
// the bundle's — so the resume proceeds through the ordinary check (the
// engine repeats it under the run lock, reading the run back from the
// store) instead of demanding `--force` for a change that never happened.
// Reports whether it did; a run that recorded neither digest is left alone
// and stays refused. Logged, since a digest rewrite is a decision the
// operator must be able to find.
func MigrateLegacyBareDigest(ctx context.Context, st store.RunStore, r *store.Run, b *bundle.Bundle, promoted string, logger *iterlog.Logger) bool {
	if promoted == "" || r == nil || r.WorkflowHash == promoted || !LegacyBareDigestMatches(r, b) {
		return false
	}
	if logger == nil {
		logger = iterlog.Nop()
	}
	from := r.WorkflowHash
	r.WorkflowHash = promoted
	if err := st.SaveRun(ctx, r); err != nil {
		r.WorkflowHash = from
		logger.Warn("resume: run %s recorded the bare digest of %s from before its bundle's prompts entered the workflow digest, but the migration to the bundle's could not be saved: %v", r.ID, b.IterPath, err)
		return false
	}
	logger.Warn("resume: run %s recorded the bare digest of %s (%s) from before its bundle's prompts entered the workflow digest; migrated to the bundle's (%s) — nothing in the source changed", r.ID, b.IterPath, short(from), short(promoted))
	return true
}

func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}
