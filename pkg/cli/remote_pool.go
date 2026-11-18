package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
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
	// FallbackUse consents the pledge to the routing fallback door. The
	// pause/enable path re-sends the donor's current value so a toggle
	// never silently erases the consent (the same replace-semantics trap
	// Bots carries).
	FallbackUse bool `json:"fallback_use,omitempty"`
	// KeyID names WHICH of the donor's keys is lent (api_key only).
	KeyID string `json:"key_id,omitempty"`
}

// poolPledgeView is the slice of the response the CLI reads back when it
// needs the CURRENT terms (pause must not silently reset the ceilings a
// donor set).
type poolPledgeView struct {
	// ID addresses THIS contribution: a donor lending two keys of one
	// provider has two pledges that the (source, ref) pair cannot tell
	// apart, and pause/withdraw must name one.
	ID          string           `json:"id"`
	Source      string           `json:"source"`
	Ref         string           `json:"ref"`
	KeyID       string           `json:"key_id,omitempty"`
	Enabled     bool             `json:"enabled"`
	Limits      credpool.Limits  `json:"limits"`
	Window      *credpool.Window `json:"window,omitempty"`
	Bots        []string         `json:"bots,omitempty"`
	FallbackUse bool             `json:"fallback_use,omitempty"`
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
func RemotePoolPause(ctx context.Context, c *RemoteClient, p *Printer, src, ref, keyID string, enabled bool) error {
	if err := validPoolCredential(src, ref); err != nil {
		return err
	}
	current, err := myPledge(ctx, c, src, ref, keyID)
	if err != nil {
		return err
	}
	if current == nil {
		if keyID != "" {
			return fmt.Errorf("you have no %s/%s contribution lending key %s", src, ref, keyID)
		}
		return fmt.Errorf("you have no %s/%s contribution to change", src, ref)
	}
	return RemotePoolShare(ctx, c, p, src, ref, PoolPledgeInput{
		Enabled:     enabled,
		Limits:      current.Limits,
		Window:      current.Window,
		Bots:        current.Bots,
		FallbackUse: current.FallbackUse,
		KeyID:       current.KeyID,
	})
}

// RemotePoolWithdraw removes one contribution entirely, addressed by its
// pledge id — the only name that picks one of two keys of one provider.
func RemotePoolWithdraw(ctx context.Context, c *RemoteClient, p *Printer, src, ref, keyID string) error {
	if err := validPoolCredential(src, ref); err != nil {
		return err
	}
	current, err := myPledge(ctx, c, src, ref, keyID)
	if err != nil {
		return err
	}
	if current == nil {
		if keyID != "" {
			return fmt.Errorf("you have no %s/%s contribution lending key %s", src, ref, keyID)
		}
		return fmt.Errorf("you have no %s/%s contribution to withdraw", src, ref)
	}
	return RemoteSendPrint(ctx, c, p, "DELETE", "/api/me/pool/entries/"+url.PathEscape(current.ID), nil)
}

// myPledge resolves the ONE stored contribution a (source, ref, key)
// names. keyID empty selects every key of the provider: a single match is
// that pledge, several are refused rather than guessed (the donor must say
// which key they mean).
func myPledge(ctx context.Context, c *RemoteClient, src, ref, keyID string) (*poolPledgeView, error) {
	raw, err := c.Call(ctx, "GET", "/api/me/pool", nil, nil)
	if err != nil {
		return nil, err
	}
	var body struct {
		Pledges []poolPledgeView `json:"pledges"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("decode pledges: %w", err)
	}
	matches := make([]*poolPledgeView, 0, 1)
	for i := range body.Pledges {
		pv := &body.Pledges[i]
		if pv.Source != src || pv.Ref != ref {
			continue
		}
		if keyID != "" && pv.KeyID != keyID {
			continue
		}
		matches = append(matches, pv)
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("you lend %d %s/%s keys — name the one you mean with --key-id", len(matches), src, ref)
	}
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
	Name    *string `json:"name,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`
	// The DOMAIN type, not a wire mirror of it — same rule as the
	// ceilings/window on PoolPledgeInput: a second copy of the shape is
	// a second thing to keep in step every time it changes, and the
	// server decodes into this exact type (poolRequest).
	Audience *credpool.Audience `json:"audience,omitempty"`
}

// RemotePoolPolicy creates or updates the pool of the team's org. The
// pool document is keyed by ORG, so this is an org-level decision — the
// server enforces that; the CLI says so in its help rather than letting
// an admin discover it through a 403.
func RemotePoolPolicy(ctx context.Context, c *RemoteClient, p *Printer, teamID string, pol PoolPolicy) error {
	if pol.Enabled == nil {
		// A PUT that CREATES the pool defaults its master switch to
		// off — a pool the broker's ListEnabled never reads, so no
		// pledge under it can ever serve and nothing says why. Probe:
		// an absent pool (the server describes "the pool that would be
		// created" with an empty id) demands the switch stated
		// explicitly rather than a silent dead-on-arrival create.
		path := "/api/teams/" + teamID + "/pool"
		status, body, err := c.API(ctx, "GET", path, nil)
		if err != nil {
			return fmt.Errorf("probe pool before policy write: %w", err)
		}
		if status < 200 || status >= 300 {
			return fmt.Errorf("probe pool before policy write: %w", &APIError{Status: status, Method: "GET", Path: path, Body: string(body)})
		}
		var v struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &v); err != nil {
			return fmt.Errorf("probe pool before policy write: decode: %w", err)
		}
		if v.ID == "" {
			return fmt.Errorf("no pool exists for this org yet — creating one requires the master switch stated explicitly: rerun with --enabled (or --enabled=false)")
		}
	}
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
