# ADR-134 — The identity tree is read, never trusted, and credentials derive only where the chokepoint asks

- **Status**: Proposed (2026-10-23; lands with the #1668 PR and is reviewed there. The operator arbitrated the doctrine across the loop: the identity tree read separately (round 1), the 30 s reuse grace (round 1), lineage revocation (round 2), everything in one PR behind one chokepoint (round 2), the chokepoint extended route by route rather than a revoking middleware (round 6), the super-admin flag re-derived at the identity boundary rather than per route (round 7))
- **Date**: 2026-10-23
- **Deciders**: jo (six arbitrations of 2026-10-20/22), Claude (design, the 9-round adversarial loop)
- **Serves**: [#1668](https://github.com/SocialGouv/iterion/issues/1668); the follow-on tickets #2687 (the remaining mints), #2655 (the read-KO class), #2674 (teams switch by browser approval)
- **Companion**: [ADR-135](135-a-clients-sign-out-is-a-machine-not-a-flag.md) — the client contract this design assumes

## Context

The org tree was a lie three ways: the auth routes built it from per-document decodes that could half-fail (#2624's class), a revoked access token kept exercising the authority it named for the rest of its 15 minutes, and every round of the loop found the same sentence false at a new site — a super-admin claim read from the token where the docs promised the account (marketplace moderation, org placement, the global memory space, another account's PAT, the org-SSO proof bypass, the team-RBAC escape hatches). The question the operator answered in round 6: revoke at the middleware, or name the routes that must not trust a stale token?

## Decision

1. **The identity tree is read, not built, by auth.** `/api/me` is the sole builder of the org tree; the routes that mint or change credentials never construct one (a half-decoded tree cannot leak into a decision), and the studio renders "identity unavailable" rather than a partial tree.
2. **Access tokens stay stateless JWTs.** A revoked session's access token keeps its ordinary access until its own expiry. The operator rejected middleware revocation for everything: the residual window is documented, not fought per request. The docs list what persists (team, org and account state; runs; bots) and what does not.
3. **The chokepoint.** Every route that MINTS a credential, GRANTS or EXERCISES platform authority, or CHANGES/REMOVES a credential asks `auth.Service.CheckDerivation` first — the switches, token mint (before and after the row write), the WS ticket (mint and redemption), the SSO link (start, and the callback before AND after the write), the credential removals and password change, revoke-all, the org SSO provider/domain writes, the desktop exchange. The families are listed in docs/cloud-admin.md §2; a new route of these families is a review finding, not a judgement call.
4. **Platform authority is re-derived, not carried.** Where the request's identity is established, a token CLAIMING super-admin gets the flag from the store and its standing (`standing && user flag`; a store-read failure is an explicit 5xx, never a silent strip; a non-claiming token pays nothing). Promotion applies at once; a pre-promotion token stays un-elevated until refresh (fail-closed). The team-RBAC escape hatches read the same stripped flag — a demoted super-admin's reach over every team ends with the demotion, not the token.
5. **Reuse is compromise.** A refresh token presented twice inside the 30 s reuse grace (`ITERION_REFRESH_REUSE_GRACE`) revokes the session's whole LINEAGE (RFC 9700). `ClaimSession` never deletes; revocation records carry a generation and do not expire. A credential change revokes before AND after its commit, so a lost commit answer still ends the old credential.
6. **SSO links are a session's credential.** A link made from the account settings records the lineage that made it; sign-out-everywhere, an admin disable and reuse detection remove those links — never the account's last way in (keep-one preserves the oldest session-written link of a password-less account; the last sign-in method refuses removal with `409 last_sign_in_method`). Links written by an SSO sign-in carry no lineage and stay. The link list shows which links a sign-out-everywhere removes (`session_linked`).
7. **PATs are pinned credentials.** Sign-out-everywhere and an admin disable revoke them; a password change does not. A PAT mints nothing: no second PAT, no session through a credential change (403 at its own temp password), no exercise of platform authority beyond what its account may. WS tickets die with their PAT; a live WebSocket re-derives standing AND scope on every mutating envelope and per tick (the bridge and the shell fail closed — a lost scope closes them); the run socket has a stated lifetime (`ITERION_RUN_WS_MAX_LIFETIME`).
8. **The wire tells the client its bound.** `GET /api/auth/me` carries the session's `expires_at` (the session row's bound, never the 15-minute access expiry), so clients can hold an honest "sign-out not confirmed" state without guessing.

## Consequences

- The residual window is a documented fact, not a bug: everything the token does outside the families persists — the docs say so plainly, and the PR body repeats it.
- Pre-lineage SSO links (before this release) are kept; admins are asked to review them once at rollout.
- The rollout constraint from the round-4 notes holds: `PATCH status=pending` ends sessions at the next refresh, old pods' mints during a rolling update are the named overlap, and Recreate with `rollingUpdate: null` is the safe path.
- The REST breakage (logout binding, new 409/403 families, `X-Iterion-Logout`) is documented without a `!` — migration notes in docs/cloud-rest-api.md.
- New auth surfaces start from the families list: a route that mints, grants or removes belongs behind the chokepoint, and its sentence goes in §2 the day it lands.
