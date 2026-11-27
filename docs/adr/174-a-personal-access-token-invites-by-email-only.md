# ADR-174 — A personal access token invites by email only

- **Status**: Accepted — the operator decided it on 2026-11-26 (SocialGouv/iterion#3744)
- **Date**: 2026-11-26
- **Deciders**: jo (the decision), Claude (the mint and its guards)
- **Relates to**: [ADR-048](048-org-team-hierarchy.md) (an invitation names its org role, same change) · [ADR-156](156-bearer-token-hashing-doctrine.md) (the invitation token is stored hashed)
- **Serves**: [#3744](https://github.com/SocialGouv/iterion/issues/3744)

## Context

An invitation token is a team token (`auth.MintTeamToken`). It outlives the
credential that minted it, and accepting it grants a team role and an org
membership. The derivation chokepoint (`auth.Service.CheckDerivation`) refuses
every team token to a personal access token (PAT). A PAT is pinned to one team
and to its own expiry, and a token handed back to it would escape both.

The CLI only ever stores a PAT. So `iterion remote teams|orgs invitations
create` was refused before anything was sent, and adding a newcomer meant
opening the studio: an operator could not invite from a terminal, and neither
could an agent working for them. iterion.cloud has a mailer
(`email_enabled: true`).

## Decision

A PAT may mint **one** team-scoped thing: an invitation **delivered by email
only** (`auth.MintEmailedInvitation`).

- **The token never comes back to the PAT.** `auth.Service.CreateEmailedInvitation`
  returns a `SealedInvitation`, whose token is unexported. The server holds
  the row to confirm its caps, and `DeliverInvitation` sends the token to the
  invited address after those confirmations. The response carries
  `"delivery": "email"` and no token.
- **Accepting stays bound to the address.** `Invitation.AddressedTo` is
  unchanged, so the PAT holds nothing it could join with.
- **It lives no longer than the PAT.** It expires no later than the PAT (a
  PAT with no expiry leaves the invitation's own TTL). The row also names the
  PAT (`Invitation.MintedWith`). The lookup and the accept refuse it with a
  410 (`ErrInvitationCredentialGone`) once that PAT does not stand: revoked,
  disabled, expired, or its owner's account disabled, erased or pending a
  password change (temporarily, for a disabled PAT and in that last case: the
  invitation is accepted again once the PAT is enabled or the account is
  active — a disable sweeps nothing — except an accept already in flight
  when the PAT stopped standing: the accept claims the invitation before it
  reads the PAT again, the re-read refuses it, and that link is spent). The
  410 does not say which.
- **Revoking a PAT ends its invitations, and the revoke names them.**
  - `DELETE /api/me/tokens/{id}` takes the PAT's pending invitations out of
    the lists and answers 200 `invitations_revoked`, naming who to invite
    again (204 when there were none). A credential pinned to one team may
    revoke any token of its account, but the invitations in teams its pin
    does not reach are counted (`invitations_revoked_elsewhere`), not named.
  - `iterion remote teams switch` has the token it replaces revoke itself,
    so the list names what that token sent, and prints it; `iterion remote
    tokens revoke` prints it, and the studio's access-token panel shows it
    once the revoke has landed.
  - A regenerate (`POST /api/me/tokens/{id}/regenerate`) revokes the token
    it replaces through that same path, so its answer names them too; a purge
    of a revoked token finishes that sweep before the row goes, and one that
    cannot finish keeps the row and names what it did end.
  - A bulk revocation (sign out everywhere, an admin disable) leaves the rows
    listed, and the lookup refuses them before any registration form.
  - Revoking a leaked token ends what it sent.
- **The rank caps are the same as any invitation's**:
  `requireRankToInvite`, its post-mint confirmations and the sweeps.
- **No mailer, no mint.** The request is refused with a 403 that points at the
  studio. If the relay refuses the email, the invitation is taken back and the
  request answers 502.

The invitation routes ask the chokepoint for the mint the presented
credential may make (`gateInvitationMint`). Each primitive demands its own
verdict, so a PAT request never reaches the primitive that returns the token.
A studio session keeps the link: the token is in the response and is emailed
too when there is a mailer. The webhook and share-link mints are unchanged,
and a PAT still cannot make them.

## Consequences

- An operator or an agent invites from the CLI in one line. The invitation
  lands in the invitee's mailbox, and nobody copies a link.
- **Residual risk:** a leaked PAT can invite an address its thief controls,
  within that PAT's own rank caps and its own team pin (the org route refuses a
  team other than the pinned one). It is the only way a non-super-admin PAT
  seats someone outside the org: a roster write from such a PAT places
  existing org members only (`teams members add` answers 422 for an
  outsider, and `orgs members add` is super-admin, whose PAT places anyone
  already). The window closes with the PAT, since an accept refuses an
  invitation whose PAT is revoked, expired or disabled. A request already in
  flight at the instant of the revoke reads the PAT's standing again after its
  own write:
  - **The mint** reads it before its email leaves. The revoke is written
    before its sweep lists the pending rows, so either that read sees the
    revoke — the row is taken back, nothing is sent, the minter is answered
    401 — or the sweep sees the row and names it: a revoke landing while the
    email is being sent is that second case.
  - **The accept** reads it once its claim has landed, and that read is its
    commit point. One that sees the revoke writes no access and answers 410;
    one past it is an accept the revoke came too late for, and it is seated,
    as a membership accepted before the revoke stays. Either way its row is
    claimed, not pending, so the sweep names it to nobody: the revoker learns
    nothing of an accept already in flight.

  The audit line of the mint names the PAT (`minted_with`).
- The CLI no longer refuses invitations client-side
  (`RefuseTeamTokenMintFromPAT` keeps the webhook mints). Against an older
  instance, its server refusal comes back instead.

## Alternatives considered

- **Browser approval** (the `teams switch` flow): a live session mints after a
  click. It keeps the doctrine whole, but it is unusable by scripts and agents.
- **Studio only**: no new surface, but "invite easily" would only be true in a
  browser.
