package native

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// Adapter exposes a *Store under the tracker.Tracker interface so the
// dispatcher can dispatch native issues with the same code path that
// drives external trackers (GitHub, Forgejo).
type Adapter struct {
	store *Store
}

// NewAdapter wraps the store as a tracker.Tracker.
func NewAdapter(store *Store) *Adapter { return &Adapter{store: store} }

// Name implements tracker.Tracker.
func (a *Adapter) Name() string { return "native" }

// ListCandidates returns unclaimed issues whose state is marked
// eligible on the board, excluding those whose blockers are not all
// terminal. Missing blockers are treated as open.
func (a *Adapter) ListCandidates(ctx context.Context) ([]tracker.Issue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b := a.store.Board()
	eligible := make([]string, 0, len(b.States))
	terminal := make(map[string]bool, len(b.States))
	for _, s := range b.States {
		if s.Eligible {
			eligible = append(eligible, s.Name)
		}
		if s.Terminal {
			terminal[s.Name] = true
		}
	}
	if len(eligible) == 0 {
		return nil, nil
	}
	free := false
	issues, err := a.store.List(ListFilter{States: eligible, Claimed: &free})
	if err != nil {
		return nil, err
	}
	out := make([]tracker.Issue, 0, len(issues))
	for _, iss := range issues {
		if a.hasOpenBlocker(iss.Blockers, terminal) {
			continue
		}
		out = append(out, toTrackerIssue(iss))
	}
	return out, nil
}

func (a *Adapter) hasOpenBlocker(blockers []string, terminal map[string]bool) bool {
	for _, id := range blockers {
		iss, err := a.store.Get(id)
		if err != nil {
			return true
		}
		if !terminal[iss.State] {
			return true
		}
	}
	return false
}

// RefreshStates returns the current state for each requested ID;
// missing IDs are omitted.
func (a *Adapter) RefreshStates(ctx context.Context, ids []string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		iss, err := a.store.Get(id)
		if err != nil {
			continue
		}
		out[id] = iss.State
	}
	return out, nil
}

// UpdateState delegates to the store.
func (a *Adapter) UpdateState(ctx context.Context, id, newState string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := a.store.SetState(id, newState)
	return err
}

// Comment is not yet supported by the native tracker (v1).
func (a *Adapter) Comment(ctx context.Context, id, body string) error {
	return fmt.Errorf("%w: native comments", tracker.ErrNotSupported)
}

// Claim delegates to the store.
func (a *Adapter) Claim(ctx context.Context, id, marker string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return a.store.Claim(id, marker)
}

// Release delegates to the store.
func (a *Adapter) Release(ctx context.Context, id, marker string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return a.store.Release(id, marker)
}

func toTrackerIssue(iss *Issue) tracker.Issue {
	return tracker.Issue{
		ID:            iss.ID,
		Identifier:    shortIdentifier(iss.ID),
		Title:         iss.Title,
		Body:          iss.Body,
		WorkflowState: iss.State,
		Priority:      iss.Priority,
		CreatedAt:     iss.CreatedAt,
		UpdatedAt:     iss.UpdatedAt,
		Labels:        append([]string(nil), iss.Labels...),
		Assignee:      iss.Assignee,
		Blockers:      append([]string(nil), iss.Blockers...),
		Fields:        cloneAnyMap(iss.Fields),
		Bot:           iss.Bot,
		BotArgs:       cloneStringMap(iss.BotArgs),
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func shortIdentifier(id string) string {
	if len(id) <= 15 {
		return id
	}
	return id[:15]
}

func cloneAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
