package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// `iterion remote board bind|show|unbind` — the team ⇄ forge project-board
// binding (ADR-097 §4), over /api/teams/{id}/board-binding.

// RemoteBoardBindOptions carries the bind flags.
type RemoteBoardBindOptions struct {
	Team string
	// Project is the board as "<owner>/<number>" (e.g. SocialGouv/203).
	Project string
	// OwnerKind is "org" (default) or "user".
	OwnerKind string
	// Connection is the forge connection id supplying the credential.
	Connection string
	// StatusMap overrides the shipped column vocabulary:
	// "Todo=ready,Doing=in_progress,Shipped=done".
	StatusMap string
	// SyncEvery is the reconciliation interval ("2m", "0" = off). Empty
	// leaves the server default.
	SyncEvery string
}

// ParseStatusMapFlag parses `--status-map "Todo=ready,Doing=in_progress"`.
//
// It is deliberately strict: a pair it could not parse would leave that
// column unmapped and therefore INERT, which looks like a working binding
// until someone notices a column that never syncs. An empty flag means "no
// override", not "an empty map".
func ParseStatusMapFlag(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		p := strings.TrimSpace(pair)
		if p == "" {
			return nil, fmt.Errorf("status map: empty entry in %q (expected Column=state pairs)", s)
		}
		col, state, ok := strings.Cut(p, "=")
		col, state = strings.TrimSpace(col), strings.TrimSpace(state)
		if !ok || col == "" || state == "" {
			return nil, fmt.Errorf("status map: %q is not a Column=state pair", p)
		}
		if strings.Contains(state, "=") {
			return nil, fmt.Errorf("status map: %q has more than one '=' (a column name containing '=' is not supported)", p)
		}
		if prev, dup := out[col]; dup {
			return nil, fmt.Errorf("status map: column %q named twice (→ %q and %q)", col, prev, state)
		}
		out[col] = state
	}
	return out, nil
}

// parseSyncEveryFlag parses `--sync-every`. Absent = the server default; "0"
// or "off" = disabled. A pointer, because those are different answers and a
// bare 0 cannot tell them apart.
func parseSyncEveryFlag(s string) (*int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil, nil
	}
	if t == "0" || strings.EqualFold(t, "off") {
		zero := int64(0)
		return &zero, nil
	}
	d, err := time.ParseDuration(t)
	if err != nil {
		return nil, fmt.Errorf("--sync-every must be a duration like 2m, or 0/off to disable, got %q", s)
	}
	if d < 0 {
		return nil, fmt.Errorf("--sync-every must be >= 0, got %q", s)
	}
	secs := int64(d / time.Second)
	return &secs, nil
}

// RemoteBoardBind binds (or re-binds) the team's project board.
func RemoteBoardBind(ctx context.Context, c *RemoteClient, p *Printer, opts RemoteBoardBindOptions) error {
	team, err := c.ResolveTeam(ctx, opts.Team)
	if err != nil {
		return err
	}
	ref, err := forge.ParseProjectRef(opts.Project)
	if err != nil {
		return fmt.Errorf("--project must be <owner>/<number> (e.g. SocialGouv/203): %w", err)
	}
	if strings.TrimSpace(opts.Connection) == "" {
		return fmt.Errorf("--connection is required (the forge connection whose credential reads the board; `iterion remote forge connections` lists them)")
	}
	statusMap, err := ParseStatusMapFlag(opts.StatusMap)
	if err != nil {
		return err
	}
	syncEvery, err := parseSyncEveryFlag(opts.SyncEvery)
	if err != nil {
		return err
	}
	req := map[string]any{
		"owner":         ref.Owner,
		"number":        ref.Number,
		"connection_id": strings.TrimSpace(opts.Connection),
	}
	if k := strings.TrimSpace(opts.OwnerKind); k != "" {
		req["owner_kind"] = k
	}
	if len(statusMap) > 0 {
		req["status_map"] = statusMap
	}
	if syncEvery != nil {
		req["sync_every_seconds"] = *syncEvery
	}
	var b forge.BoardBinding
	if _, err := c.Call(ctx, "PUT", "/api/teams/"+team+"/board-binding", req, &b); err != nil {
		return err
	}
	if p.Format == OutputJSON {
		p.JSON(b)
		return nil
	}
	printBoardBinding(p, b)
	return nil
}

// RemoteBoardShow prints the team's binding.
func RemoteBoardShow(ctx context.Context, c *RemoteClient, p *Printer, team string) error {
	t, err := c.ResolveTeam(ctx, team)
	if err != nil {
		return err
	}
	path := "/api/teams/" + t + "/board-binding"
	if p.Format == OutputJSON {
		return RemoteGetPrint(ctx, c, p, path)
	}
	var b forge.BoardBinding
	if _, err := c.Call(ctx, "GET", path, nil, &b); err != nil {
		return err
	}
	printBoardBinding(p, b)
	return nil
}

// RemoteBoardUnbind removes the team's binding.
func RemoteBoardUnbind(ctx context.Context, c *RemoteClient, p *Printer, team string) error {
	t, err := c.ResolveTeam(ctx, team)
	if err != nil {
		return err
	}
	if _, err := c.Call(ctx, "DELETE", "/api/teams/"+t+"/board-binding", nil, nil); err != nil {
		return err
	}
	p.Line("Unbound the project board from team %s", t)
	return nil
}

// printBoardBinding renders a binding for a human. It shows the EFFECTIVE
// status map, not the default: what a deployment actually runs must be
// readable, not inferred from the absence of an override.
func printBoardBinding(p *Printer, b forge.BoardBinding) {
	title := b.ProjectTitle
	if title == "" {
		title = b.Ref().String()
	}
	p.Header(title)
	p.KV("Team", b.TenantID)
	p.KV("Board", string(b.Provider)+" "+b.Ref().String()+" ("+string(b.OwnerKind.OrDefault())+")")
	if b.ProjectURL != "" {
		p.KV("URL", b.ProjectURL)
	}
	p.KV("Connection", b.ConnectionID)
	switch {
	case b.SyncEvery <= 0:
		p.KV("Reconcile", "off (the reflect has no net — re-bind with --sync-every to arm it)")
	default:
		p.KV("Reconcile", "every "+b.SyncEvery.String())
	}
	if !b.LastSyncedAt.IsZero() {
		p.KV("Last synced", b.LastSyncedAt.Format(time.RFC3339))
	}
	if b.Degraded() {
		// A partial outage, and the one thing an operator has to act on: the
		// reason names the column to re-create, or to re-bind around.
		since := ""
		if b.DegradedAt != nil {
			since = " (since " + b.DegradedAt.Format(time.RFC3339) + ")"
		}
		p.KV("Degraded", b.DegradedReason+since)
	}
	p.Blank()
	p.Line("Status map:")
	if b.StatusFieldID == "" {
		p.Line("  (this board has no Status field — labels only, no status projection)")
	}
	for _, m := range b.Mapping() {
		opt := b.StatusOptions[m.State]
		mark := "  "
		if opt == "" {
			mark = "! " // present in the map, absent from the board
		}
		p.Line("%s%-14s → %s", mark, m.Status, m.State)
	}
	if len(b.MissingStatuses) > 0 {
		p.Line("  ! columns absent from the board: %s", strings.Join(b.MissingStatuses, ", "))
	}
	if len(b.LabelFields) > 0 {
		p.Blank()
		p.Line("Label fields:")
		for _, f := range b.LabelFields {
			p.Line("  %-14s → %s<value>", f.Name, f.Prefix)
		}
	}
}
