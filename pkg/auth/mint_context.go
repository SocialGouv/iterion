package auth

import (
	"context"
	"fmt"
)

// MintVerdictMissingError is a team-token mint primitive's refusal: the
// call's context carries no derivation verdict for the mint asked, so no
// credential was approved to mint. The verdict travels in the request's
// context — the derivation gate stamps it once its ask grants
// (ContextWithMintVerdict) — and the primitive refuses without it, fail
// closed, whatever the caller above it, its handler, its shape or its
// branch.
type MintVerdictMissingError struct {
	// Primitive is the name of the primitive that refused.
	Primitive string
	// Mint is the mint the verdict was required for.
	Mint Mint
}

func (e *MintVerdictMissingError) Error() string {
	return fmt.Sprintf("auth: %s refused: the context carries no derivation verdict for %s — ask the derivation chokepoint (CheckDerivation) and carry its verdict in the context", e.Primitive, e.Mint)
}

// mintVerdictKey is the private key the derivation verdict travels by in a
// request's context. Only this package stamps and reads it.
type mintVerdictKey struct{}

// MintVerdict is the derivation chokepoint's verdict as it travels in a
// request's context: the mint the ask was about, and the credential it
// approved. A verdict for one mint approves nothing else (a personal
// access token's MintPAT grant mints no team token), and the zero value
// approves nothing at all. The fields are unexported, so a verdict exists
// only where this package issued one (NewMintVerdict) — a caller cannot
// fabricate one.
type MintVerdict struct {
	mint         Mint
	credentialID string
}

// Credential answers the credential the ask approved, for an audit line the
// minter keeps.
func (v MintVerdict) Credential() string { return v.credentialID }

// NewMintVerdict issues the verdict a derivation ask that granted may stamp
// into a request's context. The derivation gate calls it right after its ask
// (CheckDerivation) grants; production code anywhere else is a trapdoor, and
// TestEveryTeamTokenMintVerdictStamperIsTheGateOrATest (pkg/server) refuses
// one — in every spelling source can reach this constructor by: qualified,
// a bare Ident through a dot-import of this package, or a MintVerdict
// literal with the private key by hand. Tests issue verdicts through this
// same constructor, so the context a test mints under is the context the
// gate stamps, never a side door.
func NewMintVerdict(mint Mint, credentialID string) MintVerdict {
	return MintVerdict{mint: mint, credentialID: credentialID}
}

// ContextWithMintVerdict returns ctx carrying the verdict. Stamping without
// the ask is the trapdoor NewMintVerdict's doc names; the same inventory
// test holds the stamp — and the raw routes beside it (a MintVerdict
// literal, the private key in a bare context.WithValue) — to the gate.
func ContextWithMintVerdict(ctx context.Context, v MintVerdict) context.Context {
	return context.WithValue(ctx, mintVerdictKey{}, v)
}

// MintVerdictFromContext answers the verdict ctx carries for mint, and
// whether it approves that mint. A verdict carried for another mint — and
// the zero value — approve nothing.
func MintVerdictFromContext(ctx context.Context, mint Mint) (MintVerdict, bool) {
	if ctx == nil {
		return MintVerdict{}, false
	}
	v, ok := ctx.Value(mintVerdictKey{}).(MintVerdict)
	if !ok || v.mint != mint {
		return MintVerdict{}, false
	}
	return v, true
}

// RequireMintVerdict is the requirement the team-token mint primitives
// enforce first thing in their body (webhooks.MintToken, configshare.MintToken,
// Service.CreateInvitation, and the forge orchestrator's hook-identity
// reservation through webhooks.MintToken): the context must carry the
// chokepoint's verdict for mint, or the call is refused — fail closed,
// whatever the caller, its handler, its shape or its branch. The refusal is
// a *MintVerdictMissingError.
func RequireMintVerdict(ctx context.Context, mint Mint, primitive string) (MintVerdict, error) {
	v, ok := MintVerdictFromContext(ctx, mint)
	if !ok {
		return MintVerdict{}, &MintVerdictMissingError{Primitive: primitive, Mint: mint}
	}
	return v, nil
}
