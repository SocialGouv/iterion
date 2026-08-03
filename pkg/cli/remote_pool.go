package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/credpool"
)

// Credential-pool CLI: lending an LLM subscription to the shared pool and
// watching what it served. The credential itself is never touched here —
// it was connected once through the OAuth flow and stays sealed
// server-side; what these commands set are the donor's TERMS.

// PoolPledgeInput is the PUT body of /api/me/pool/{kind}. The ceilings and
// window are the DOMAIN types, not a wire mirror of them: a second copy of
// the shape is a second thing to keep in step every time it changes.
type PoolPledgeInput struct {
	Enabled bool             `json:"enabled"`
	Limits  credpool.Limits  `json:"limits"`
	Window  *credpool.Window `json:"window,omitempty"`
	Bots    []string         `json:"bots,omitempty"`
}

// poolPledgeView is the slice of the response the CLI reads back when it
// needs the CURRENT terms (pause must not silently reset the ceilings a
// donor set).
type poolPledgeView struct {
	Kind    string           `json:"kind"`
	Enabled bool             `json:"enabled"`
	Limits  credpool.Limits  `json:"limits"`
	Window  *credpool.Window `json:"window,omitempty"`
	Bots    []string         `json:"bots,omitempty"`
}

// RemotePoolStatus prints the caller's own contributions and what they
// have served.
func RemotePoolStatus(ctx context.Context, c *RemoteClient, p *Printer) error {
	return RemoteGetPrint(ctx, c, p, "/api/me/pool")
}

// RemotePoolHistory prints what the caller's quota actually ran.
func RemotePoolHistory(ctx context.Context, c *RemoteClient, p *Printer) error {
	return RemoteGetPrint(ctx, c, p, "/api/me/pool/history")
}

// RemotePoolShare creates or updates a contribution.
func RemotePoolShare(ctx context.Context, c *RemoteClient, p *Printer, kind string, in PoolPledgeInput) error {
	if kind == "" {
		return fmt.Errorf("--kind is required (claude_code|codex)")
	}
	raw, err := c.Call(ctx, "PUT", "/api/me/pool/"+kind, in, nil)
	if err != nil {
		return err
	}
	PrintRemoteJSON(p, raw)
	return nil
}

// RemotePoolPause flips a contribution off WITHOUT discarding its terms:
// it reads the stored pledge back and re-sends it with enabled=false, so
// resuming later does not require the donor to retype their ceilings.
func RemotePoolPause(ctx context.Context, c *RemoteClient, p *Printer, kind string, enabled bool) error {
	if kind == "" {
		return fmt.Errorf("--kind is required (claude_code|codex)")
	}
	raw, err := c.Call(ctx, "GET", "/api/me/pool", nil, nil)
	if err != nil {
		return err
	}
	var body struct {
		Pledges []poolPledgeView `json:"pledges"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("decode pledges: %w", err)
	}
	var current *poolPledgeView
	for i := range body.Pledges {
		if body.Pledges[i].Kind == kind {
			current = &body.Pledges[i]
			break
		}
	}
	if current == nil {
		return fmt.Errorf("you have no %s contribution to change", kind)
	}
	return RemotePoolShare(ctx, c, p, kind, PoolPledgeInput{
		Enabled: enabled,
		Limits:  current.Limits,
		Window:  current.Window,
		Bots:    current.Bots,
	})
}

// RemotePoolWithdraw removes a contribution entirely.
func RemotePoolWithdraw(ctx context.Context, c *RemoteClient, p *Printer, kind string) error {
	if kind == "" {
		return fmt.Errorf("--kind is required (claude_code|codex)")
	}
	return RemoteSendPrint(ctx, c, p, "DELETE", "/api/me/pool/"+kind, nil)
}

// RemotePoolDonors prints the operator view: the pool's policy and who is
// lending to it.
func RemotePoolDonors(ctx context.Context, c *RemoteClient, p *Printer, teamID string) error {
	return RemoteGetPrint(ctx, c, p, "/api/teams/"+teamID+"/pool")
}

// LocalTimezone returns the host's IANA zone name so a donor's sharing
// hours mean what they read on their own clock. Falls back to UTC when the
// host cannot name its zone (a bare container), which is honest: an
// unnamed zone would otherwise be silently reinterpreted.
func LocalTimezone() string {
	if loc := time.Local.String(); loc != "" && loc != "Local" {
		return loc
	}
	return "UTC"
}
