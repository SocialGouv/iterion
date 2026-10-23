# ADR-135 — A client's sign-out is a machine, not a flag: OWED until the server confirms, abandoned only in the open

- **Status**: Proposed (2026-10-23; lands with the #1668 PR and is reviewed there. The operator chose the replacement approach on 2026-10-22 after rounds 6 and 7 each regressed the previous round's sign-out patches — "stop patching; write one machine whose invariants are the tests")
- **Date**: 2026-10-23
- **Deciders**: jo (the new-approach arbitration), Claude (the machine's design, its implementation, the rounds that forced it)
- **Extends**: [ADR-134](134-identity-tree-read-separately-credentials-derive-at-the-chokepoint.md) — the session lineage the machine binds to, and `/api/auth/me`'s `expires_at` it holds
- **Serves**: [#1668](https://github.com/SocialGouv/iterion/issues/1668); every future client surface (CLI, desktop, a new pane)

## Context

A browser cannot end its own httpOnly session: "sign out" is a request, and until the server confirms it the session is live. Two rounds of fixes for the honesty problem (round 5: hold a pending mark; round 6: bind it to a lineage, persist it with the keychain, present it before a new sign-in) each fixed their finding and regressed the neighbours — "Continue anyway" silently dropped the other tabs' warning while the session lived; a keychain-locked hold was lost at the next sign-in; a project switch ran a second renewal jar whose stale tick revoked the session the operator had just switched to. The operator's ruling: replace the patches with ONE written machine whose invariants are the tests.

## Decision

1. **One record, written before the wire.** The studio holds a single persisted record `{state: owed|abandoned, lineage, account, expires_at, since, abandoned_at}`; the desktop one keychain entry per connection carries the same shape beside the jar. The record is written BEFORE the logout request — a crash in flight cannot lose the ask. The studio's states: `IDLE`, `OWED` (+`HELD`|`GONE`|`UNKNOWN`), `ABANDONED` (+`HELD`|`GONE`).
2. **The invariants are the contract.**
   - **I1** — the UI never shows "signed out", and no tab loses the "sign-out not confirmed" warning, while a credential the client holds is live as far as it knows.
   - **I2** — no path abandons a pending sign-out without telling the user, in every tab, that the session is still active (with its expiry when known). `ABANDONED` is a state the user reached by an explicit, warned action — never a side effect.
   - **I3** — a pending sign-out never ends a session other than the one it names, except by an explicit user action that names it. Presentations read their snapshot under one lock with the token they present.
   - **I4** — a sign-in while a sign-out is pending presents it first or holds it ALONGSIDE the new session (the desktop's HELD-BESIDE in one keychain value) — never overwrites it.
   - **I5** — a crash or restart at any point preserves I1-I4.
   - **I6** — every tab and window converges to the same state (the record's storage events, a visibility/focus re-read; a tab opened after the fact reads the record).
3. **One writer per keychain entry.** The desktop's renewal loop is per connection; a jar is installed through the sign-in discipline or it does not install; a replaced jar is retired, never left ticking. A held sign-out is bound to the entry that carries it, and a tokenless hold is never presented at a token it did not name.
4. **Presentation and close.** A close (pane, connection) presents what its entry carries and then proceeds on all-confirmed — or refuses naming the hold and the remedies. Reopening never renews-and-presents a session the operator asked to end.
5. **The wire's shape is part of the contract.** Close reasons compose: `<token>: <cause>`, never a bare token — a client matches by prefix (`credential_refused`, `scope_lost`, `lifetime_reached`). `/api/auth/me` carries the session's `expires_at`, which the OWED banner shows and `expireStale` acts on; a degraded answer (no field) degrades the date, never the state.
6. **Deleted special cases stay deleted.** The unnamed re-asks, the adopt-only-with-token rule, the two-jar window, the silent abandon paths: each existed for a round-5/6 finding; the machine's witnesses re-drive each original scenario, and the old bug stays dead without its patch.

## Consequences

- A new client surface implements the machine (or a subset that keeps I1-I6 for the states it has) and inherits the test shapes; "my sign-out is honest" is asserted, not argued.
- The studio's password sign-in still abandons an unconfirmed sign-out — the browser holds one cookie — so that path presents the same warned abandon as SSO's "Continue anyway"; it is the one known I2 narrowing, stated in the machine doc.
- A presenter-election optimisation (one presenter per record across tabs) is deliberately unimplemented; the spec sentence describes the implemented truth.
- The machine's spec lives at `scratch` during review and lands as the state-machine section of the PR's docs (desktop-architecture.md, browser-pane.md carry the operator-facing halves).
