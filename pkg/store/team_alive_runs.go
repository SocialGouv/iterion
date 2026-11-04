package store

import (
	"context"
	"fmt"
	"os"
	"sort"
)

// TeamAliveRunsReader supplies the cutover backstop with a complete list of
// executing or queued runs. LLM-idle running runs still count: they can resume
// spending without going through launch admission. Parked runs do not count;
// their resume checks the immutable pool stamp against the team's mapping.
// An unreadable leg is an error, never evidence that the team is drained.
// The read is a snapshot, not a lock against concurrent launch or resume.
type TeamAliveRunsReader interface {
	ListAliveRunsForTeam(context.Context, string) ([]string, error)
}

func AsTeamAliveRunsReader(s RunStore) TeamAliveRunsReader {
	return capability[TeamAliveRunsReader](s)
}

func (s *FilesystemRunStore) ListAliveRunsForTeam(ctx context.Context, teamID string) ([]string, error) {
	if teamID == "" {
		return nil, fmt.Errorf("store: alive team runs: empty team")
	}
	if tenant, ok := TenantFromContext(ctx); ok && tenant != teamID {
		return nil, fmt.Errorf("store: alive team runs: context tenant differs from target team")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// ListRuns/filterRunsSorted deliberately skip missing or unreadable records.
	// A cutover must not turn that uncertainty into permission to change pools.
	ids, err := s.ListRunDirs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		run, err := s.LoadRun(ctx, id)
		if err != nil {
			// A concurrent completed deletion is harmless. An existing incomplete or
			// corrupt directory remains unknown, even when LoadRun says not found.
			if s.runDeleted(id) {
				continue
			}
			if _, statErr := os.Stat(s.runDir(id)); os.IsNotExist(statErr) {
				continue
			}
			return nil, fmt.Errorf("store: alive team runs: read run %s: %w", id, err)
		}
		if run.ID != id {
			return nil, fmt.Errorf("store: alive team runs: run %s has inconsistent identity %q", id, run.ID)
		}
		alive, err := TeamCutoverStatusAlive(run.Status)
		if err != nil {
			return nil, fmt.Errorf("store: alive team runs: run %s: %w", id, err)
		}
		if run.TenantID == teamID && alive {
			out = append(out, run.ID)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}
