package runview

import (
	"context"
	"regexp"
	"strings"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/internal/shellquote"
	"github.com/SocialGouv/iterion/pkg/store"
)

// WorkspaceCheckpoint is a recovery hint from a successful persisted push
// event, not a delivery bank or a claim that the remote ref still exists.
// It never changes Run.FinalBranch, FinalCommit or merge eligibility.
type WorkspaceCheckpoint struct {
	Ref          string          `json:"ref"`
	Commit       string          `json:"commit"`
	Source       store.EventType `json:"source"`
	EventSeq     int64           `json:"event_seq"`
	RecordedAt   time.Time       `json:"recorded_at"`
	FetchCommand string          `json:"fetch_command"`
	Warning      string          `json:"warning"`
}

var checkpointCommitPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

func workspaceCheckpointFromEvent(evt *store.Event) *WorkspaceCheckpoint {
	if evt == nil || evt.Type != store.EventRunWorkspaceCheckpoint {
		return nil
	}
	if _, failed := evt.Data["error"]; failed {
		return nil
	}
	ref, _ := evt.Data["ref"].(string)
	commit, _ := evt.Data["commit"].(string)
	if gitlib.ValidateBranchName(ref) != nil || !checkpointCommitPattern.MatchString(commit) {
		return nil
	}
	fetchRef := ref
	if !strings.HasPrefix(fetchRef, "refs/") {
		fetchRef = "refs/heads/" + fetchRef
	}
	return &WorkspaceCheckpoint{
		Ref: ref, Commit: commit, Source: evt.Type, EventSeq: evt.Seq, RecordedAt: evt.Timestamp,
		FetchCommand: "git fetch origin " + shellquote.Quote(fetchRef),
		Warning:      "Checkpoint only; delivery gate not established. From a clone of the run's repository (origin), verify FETCH_HEAD matches the recorded commit and validate before merging. The ref may have moved or been deleted.",
	}
}

// LoadWorkspaceCheckpoint reads only persisted events, with the caller's
// identity. A failed later push does not erase the last known success. Event
// read failures are returned rather than disguised as no recoverable work.
func LoadWorkspaceCheckpoint(ctx context.Context, s store.RunStore, runID string) (*WorkspaceCheckpoint, error) {
	if _, err := s.LoadRun(ctx, runID); err != nil {
		return nil, err
	}
	var latest *WorkspaceCheckpoint
	err := s.ScanEvents(ctx, runID, func(evt *store.Event) bool {
		if cp := workspaceCheckpointFromEvent(evt); cp != nil && (latest == nil || cp.EventSeq > latest.EventSeq) {
			latest = cp
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return latest, nil
}
