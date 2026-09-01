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
	// KeyID names WHICH of the donor's keys is lent (api_key only).
	KeyID string `json:"key_id,omitempty"`
}

// poolPledgeView is the slice of the response the CLI reads back when it
// needs the CURRENT terms (pause must not silently reset the ceilings a
// donor set).
type poolPledgeView struct {
	Source  string           `json:"source"`
	Ref     string           `json:"ref"`
	KeyID   string           `json:"key_id,omitempty"`
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
func RemotePoolShare(ctx context.Context, c *RemoteClient, p *Printer, src, ref string, in PoolPledgeInput) error {
	if err := validPoolCredential(src, ref); err != nil {
		return err
	}
	raw, err := c.Call(ctx, "PUT", "/api/me/pool/"+src+"/"+ref, in, nil)
	if err != nil {
		return err
	}
	PrintRemoteJSON(p, raw)
	return nil
}

// RemotePoolPause flips a contribution off WITHOUT discarding its terms:
// it reads the stored pledge back and re-sends it with enabled=false, so
// resuming later does not require the donor to retype their ceilings.
func RemotePoolPause(ctx context.Context, c *RemoteClient, p *Printer, src, ref string, enabled bool) error {
	if err := validPoolCredential(src, ref); err != nil {
		return err
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
		if body.Pledges[i].Source == src && body.Pledges[i].Ref == ref {
			current = &body.Pledges[i]
			break
		}
	}
	if current == nil {
		return fmt.Errorf("you have no %s/%s contribution to change", src, ref)
	}
	return RemotePoolShare(ctx, c, p, src, ref, PoolPledgeInput{
		Enabled: enabled,
		Limits:  current.Limits,
		Window:  current.Window,
		Bots:    current.Bots,
		KeyID:   current.KeyID,
	})
}

// RemotePoolWithdraw removes a contribution entirely.
func RemotePoolWithdraw(ctx context.Context, c *RemoteClient, p *Printer, src, ref string) error {
	if err := validPoolCredential(src, ref); err != nil {
		return err
	}
	return RemoteSendPrint(ctx, c, p, "DELETE", "/api/me/pool/"+src+"/"+ref, nil)
}

// validPoolCredential rejects a malformed (source, ref) before it becomes a
// confusing 404 from the server.
func validPoolCredential(src, ref string) error {
	if !credpool.CredentialSource(src).Valid() {
		return fmt.Errorf("--source must be oauth or api_key (got %q)", src)
	}
	if ref == "" {
		return fmt.Errorf("--ref is required (a subscription like claude_code, or a provider like anthropic)")
	}
	return nil
}

// RemotePoolDonors prints the operator view: the pool's policy and who is
// lending to it.
func RemotePoolDonors(ctx context.Context, c *RemoteClient, p *Printer, teamID string) error {
	return RemoteGetPrint(ctx, c, p, "/api/teams/"+teamID+"/pool")
}

// PoolPolicy is the operator-side settings of a pool: its name, its
// master switch, and who may draw on it. Every field is optional — only
// the ones the caller sets are sent, so `pool policy --enabled=false`
// pauses a pool without restating its audience.
type PoolPolicy struct {
	Name     *string `json:"name,omitempty"`
	Enabled  *bool   `json:"enabled,omitempty"`
	Audience *struct {
		Teams        []string `json:"teams,omitempty"`
		Orgs         []string `json:"orgs,omitempty"`
		Contributors bool     `json:"contributors,omitempty"`
		AllTeams     bool     `json:"all_teams,omitempty"`
	} `json:"audience,omitempty"`
}

// RemotePoolPolicy creates or updates the pool of the team's org. The
// pool document is keyed by ORG, so this is an org-level decision — the
// server enforces that; the CLI says so in its help rather than letting
// an admin discover it through a 403.
func RemotePoolPolicy(ctx context.Context, c *RemoteClient, p *Printer, teamID string, pol PoolPolicy) error {
	body, err := json.Marshal(pol)
	if err != nil {
		return fmt.Errorf("encode pool policy: %w", err)
	}
	return RemoteSendPrint(ctx, c, p, "PUT", "/api/teams/"+teamID+"/pool", body)
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
