package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// --- Personal access token lifecycle: regenerate, disable, enable, purge ---

// tokenPath is the route of one token, and of one of its acts when act is
// not empty.
func tokenPath(tokenID, act string) string {
	p := "/api/me/tokens/" + url.PathEscape(tokenID)
	if act != "" {
		p += "/" + act
	}
	return p
}

// tokenRearmAdvice names where a token is regenerated or enabled. Both put a
// credential in the caller's hands, which only a signed-in SESSION may do —
// and every credential the CLI stores is a personal access token (a `remote
// login` stores one too), so the studio is the place, not another login.
func tokenRearmAdvice(base string) string {
	return "Do it in the studio: " + strings.TrimRight(base, "/") + " → Account → Access tokens.\n" +
		"Regenerating or enabling a token takes a signed-in session; the CLI only ever\n" +
		"stores a personal access token, so no CLI login can do it."
}

// regeneratedPAT is what POST /api/me/tokens/{id}/regenerate answers.
type regeneratedPAT struct {
	PAT         remotePAT `json:"pat"`
	Token       string    `json:"token"`
	Replaced    string    `json:"replaced"`
	RevokeError string    `json:"revoke_error"`
	patRevocation
}

// RemoteTokensRegenerate replaces a token by a new one with the same name,
// team pin and expiry, prints the new plaintext once, and names the pending
// invitations the replaced token had emailed, which end with it (ADR-174).
// The instance mints the replacement BEFORE it revokes the replaced token: a
// revoke that failed comes back beside the new plaintext, which is printed
// first, and the command then fails naming the token still to revoke.
func RemoteTokensRegenerate(ctx context.Context, c *RemoteClient, p *Printer, tokenID string) error {
	advice := tokenRearmAdvice(c.BaseURL())
	if err := c.refuseIfPAT(advice); err != nil {
		return err
	}
	path := tokenPath(tokenID, "regenerate")
	code, raw, err := c.API(ctx, "POST", path, nil)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		refusal, _ := asPATMintRefusal(c.refuseAPI(code, "POST", path, raw), advice)
		return refusal
	}
	var out regeneratedPAT
	decodeErr := json.Unmarshal(raw, &out)
	if decodeErr == nil && out.Token == "" {
		decodeErr = fmt.Errorf("it carries no token")
	}
	if decodeErr != nil {
		// The plaintext exists nowhere but in this body: hand it over as it
		// came rather than lose it to a decode.
		p.Line("%s", bytes.TrimSpace(raw))
		return fmt.Errorf("the instance answered %d to the regenerate of %s with a body this CLI cannot read (printed above, it may hold the new token): %w", code, tokenID, decodeErr)
	}
	if p.Format == OutputJSON {
		PrintRemoteJSON(p, raw)
	} else {
		p.Line("Token %s regenerated as %s", tokenID, out.PAT.ID)
		p.Line("%s", out.Token)
		p.Line("(store it now — the plaintext is never shown again)")
		if out.RevokeError == "" {
			p.Line("Token %s revoked", tokenID)
			printEndedInvitations(p, out.patRevocation)
		}
	}
	if out.RevokeError != "" {
		return fmt.Errorf("%s\nrevoke it: iterion remote tokens revoke %s", out.RevokeError, tokenID)
	}
	return nil
}

// RemoteTokensDisable suspends a token: it authenticates nothing, and the
// invitations it emailed are refused until it is enabled — they are not
// ended.
func RemoteTokensDisable(ctx context.Context, c *RemoteClient, p *Printer, tokenID string) error {
	return remoteTokenStateChange(ctx, c, p, tokenID, "disable", "disabled")
}

// RemoteTokensEnable lifts a disable. A personal access token may not re-arm
// a credential, so this refuses before it sends anything when the stored
// credential is one.
func RemoteTokensEnable(ctx context.Context, c *RemoteClient, p *Printer, tokenID string) error {
	advice := tokenRearmAdvice(c.BaseURL())
	if err := c.refuseIfPAT(advice); err != nil {
		return err
	}
	err := remoteTokenStateChange(ctx, c, p, tokenID, "enable", "enabled")
	if refusal, ok := asPATMintRefusal(err, advice); ok {
		return refusal
	}
	return err
}

func remoteTokenStateChange(ctx context.Context, c *RemoteClient, p *Printer, tokenID, act, done string) error {
	var out remotePAT
	raw, err := c.Call(ctx, "POST", tokenPath(tokenID, act), nil, &out)
	if err != nil {
		return err
	}
	if p.Format == OutputJSON {
		PrintRemoteJSON(p, raw)
		return nil
	}
	p.Line("Token %s %s", tokenID, done)
	return nil
}

// RemoteTokensPurge removes the row of a REVOKED token (its audit lines
// stay); the instance refuses a live or disabled one. It finishes the
// revoke's invitation sweep first and names what that ended.
func RemoteTokensPurge(ctx context.Context, c *RemoteClient, p *Printer, tokenID string) error {
	purged, err := sendTokenSweep(ctx, c, "POST", tokenPath(tokenID, "purge"))
	if err != nil {
		// A sweep that could not finish keeps the row and names what it did
		// end: print those before failing, so none ends in silence.
		var ae *APIError
		var unfinished struct {
			ErrorCode string `json:"error_code"`
			patRevocation
		}
		if errors.As(err, &ae) && ae.Status == http.StatusConflict &&
			json.Unmarshal([]byte(ae.Body), &unfinished) == nil && unfinished.ErrorCode == "pat_sweep_unfinished" {
			if p.Format == OutputJSON {
				PrintRemoteJSON(p, []byte(ae.Body))
			} else {
				printEndedInvitations(p, unfinished.patRevocation)
			}
			return fmt.Errorf("token %s is kept, its invitation sweep could not finish — re-run `iterion remote tokens purge %s`: %w", tokenID, tokenID, err)
		}
		return err
	}
	if p.Format == OutputJSON {
		v := purged.view(tokenID)
		delete(v, "revoked")
		v["purged"] = tokenID
		p.JSON(v)
		return nil
	}
	p.Line("Token %s purged", tokenID)
	printEndedInvitations(p, purged)
	return nil
}
