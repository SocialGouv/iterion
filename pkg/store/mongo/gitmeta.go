package mongo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/store"
)

// runGitMetaDoc is the persisted snapshot of a run's git activity, one
// document per run (unique run_id). The runner pod upserts it once after
// the run returns (post-finalize); the server pod reads it back to
// render the Commits/Files panels for a run whose worktree is gone. The
// embedded gitlib types keep the wire shape identical to the live git
// path, so the HTTP handlers serve persisted and live data uniformly.
type runGitMetaDoc struct {
	TenantID        string                                   `bson:"tenant_id,omitempty"`
	RunID           string                                   `bson:"run_id"`
	BaseCommit      string                                   `bson:"base_commit,omitempty"`
	HeadCommit      string                                   `bson:"head_commit,omitempty"`
	Commits         []gitlib.CommitInfo                      `bson:"commits"`
	Files           []gitlib.FileStatus                      `bson:"files"`
	CommitFiles     map[string][]gitlib.FileStatus           `bson:"commit_files,omitempty"`
	FileDiffs       map[string]*store.RunFileDiff            `bson:"file_diffs,omitempty"`
	CommitFileDiffs map[string]map[string]*store.RunFileDiff `bson:"commit_file_diffs,omitempty"`
	DiffsTruncated  bool                                     `bson:"diffs_truncated,omitempty"`
	UpdatedAt       time.Time                                `bson:"updated_at"`
}

// SaveRunGitMeta implements store.RunGitMetaStore: upsert the whole
// snapshot keyed on run_id. Re-saving replaces the prior snapshot so the
// latest write always reflects the full base..head range.
func (s *Store) SaveRunGitMeta(ctx context.Context, runID string, meta *store.RunGitMeta) error {
	if meta == nil {
		return fmt.Errorf("store/mongo: SaveRunGitMeta(%s): nil meta", runID)
	}
	if meta.UpdatedAt.IsZero() {
		meta.UpdatedAt = time.Now().UTC()
	}
	doc := runGitMetaDoc{
		RunID:           runID,
		BaseCommit:      meta.BaseCommit,
		HeadCommit:      meta.HeadCommit,
		Commits:         meta.Commits,
		Files:           meta.Files,
		CommitFiles:     meta.CommitFiles,
		FileDiffs:       meta.FileDiffs,
		CommitFileDiffs: meta.CommitFileDiffs,
		DiffsTruncated:  meta.DiffsTruncated,
		UpdatedAt:       meta.UpdatedAt,
	}
	if id, ok := store.TenantFromContext(ctx); ok {
		doc.TenantID = id
	}
	filter := withTenantFilter(ctx, bson.M{"run_id": runID})
	if _, err := s.runGitMeta.ReplaceOne(ctx, filter, doc, options.Replace().SetUpsert(true)); err != nil {
		return fmt.Errorf("store/mongo: save git meta %s: %w", runID, err)
	}
	return nil
}

// LoadRunGitMeta implements store.RunGitMetaStore: the snapshot for the
// run, or (nil, nil) when none was ever recorded.
func (s *Store) LoadRunGitMeta(ctx context.Context, runID string) (*store.RunGitMeta, error) {
	filter := withTenantFilter(ctx, bson.M{"run_id": runID})
	var doc runGitMetaDoc
	if err := s.runGitMeta.FindOne(ctx, filter).Decode(&doc); err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, fmt.Errorf("store/mongo: load git meta %s: %w", runID, err)
	}
	return &store.RunGitMeta{
		BaseCommit:      doc.BaseCommit,
		HeadCommit:      doc.HeadCommit,
		Commits:         doc.Commits,
		Files:           doc.Files,
		CommitFiles:     doc.CommitFiles,
		FileDiffs:       doc.FileDiffs,
		CommitFileDiffs: doc.CommitFileDiffs,
		DiffsTruncated:  doc.DiffsTruncated,
		UpdatedAt:       doc.UpdatedAt,
	}, nil
}
