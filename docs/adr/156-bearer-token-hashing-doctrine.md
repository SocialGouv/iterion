# ADR-156: Bearer tokens at rest — an unsalted SHA-256 digest; HMAC is reserved for derivations

- **Status**: Accepted — the operator decided it on 2026-11-03 (DA↔repository gap 2, arbitration Q8: "the code is correct, the documentation lies"). This record changes no code path: it names the primitive the tree already uses, fixes the prose that contradicted it, and draws the line a future hash must not cross.
- **Date**: 2026-11-03
- **Relates to**: [secrets-reference.md](../secrets-reference.md) (the token table this record backs) · [docs/adr/076-scoped-config-share-editor.md](076-scoped-config-share-editor.md) (its dated addendum, an erratum) · [ADR-146](146-secret-boundary-v2-grants-and-sinks.md) (secret boundary v2 — a digest is not a sealed secret) · [data-model-knowledge.md](../diagrams/data-model-knowledge.md) and [data-model-forge-bots.md](../diagrams/data-model-forge-bots.md) (the two diagrams that already said "unsalted")

## Context

Every long-lived bearer token iterion stores — refresh token, personal
access token `iap_`, password-reset link `iar_`, team invitation, inbound
webhook `iwh_`, config-share `iws_` — goes through one primitive:
`auth.GenerateRandomToken(n)` draws `n` bytes of `crypto/rand`, and
`auth.HashRefreshToken` stores `hex(SHA-256(plaintext))` under a unique
`token_hash` index ([pkg/auth/refresh.go](../../pkg/auth/refresh.go)). The
digest is **unsalted and unkeyed**. Two diagrams said so; three pages and
four package comments said "salted". A qualification file copies what the
documentation says, so a flattering sentence becomes a false claim in front
of a security officer — and the next reader who trusts the sentence and
"fixes" the code by adding a random salt breaks the index lookup of every
token resolved by its digest.

What a salt is for. A salt defeats precomputation (rainbow tables) and makes
two equal inputs hash differently. Both defences matter for inputs an
attacker can enumerate — passwords, e-mails, short identifiers. They buy
nothing for an input the attacker cannot enumerate: a 256-bit random token
has 2^256 preimages to try, and no table covers them. A pepper (a server-side
HMAC key) adds a defence only when the database leaks WITHOUT the key; here
the signing secret and MongoDB live in the same Kubernetes namespace, so the
threat it answers is marginal, and a single-key signer
(`NewJWTSigner`, one `ITERION_JWT_SECRET`) means a key rotation would
invalidate every token at once unless the multi-key path is wired first.

The entropy of each token at the date of this record. The refresh successor
is the one row that is not raw entropy: a rotation draws a 16-byte nonce and
derives the successor by HMAC (clause 3), so its 384 bits are keyed output —
unpredictable without the server key and the presented token, not drawn from
`crypto/rand`.

| Token | Bytes of `crypto/rand` | Bits | Minted at |
|---|---|---|---|
| refresh (opaque) | 48 | 384 | `auth.IssueSession` — `refreshTokenBytes` |
| refresh successor | 16 (rotation nonce), then HMAC-SHA512 under `derivedKey(kid, refreshSuccessorLabel)` over nonce ‖ 0 ‖ presented, truncated to 48 bytes | 384 of keyed output, not raw entropy | `Service.refreshSuccessor` |
| `iap_` PAT | 32 | 256 | `pat.MintToken` |
| `iar_` reset | 32 | 256 | `Service.RequestPasswordReset` |
| invitation | 32 | 256 | `Service.CreateInvitation` |
| `iwh_` webhook | 32 | 256 | `webhooks.MintToken` |
| `iws_` config share | 32 | 256 | `configshare.MintToken` |

Passwords are the one human-chosen secret, and they are argon2id with a
per-hash salt (`auth.HashPassword`) — the right tool for a guessable input.

## Decision

1. **A bearer token at rest is its unsalted SHA-256 digest** — `hex(SHA-256(plaintext))`
   through `auth.HashRefreshToken`, stored under the unique `token_hash`
   index — refresh, reset, invitation and PAT resolve the row by that index;
   webhook and config-share fetch it by its id. This is the accepted
   primitive for every token that is **CSPRNG output of at least 128 bits**;
   the tree's tokens carry 256 or 384.
2. **Never for a human-chosen or enumerable input.** A password is argon2id
   (salted, memory-hard). An e-mail, a username, a short identifier that a
   dictionary covers is either stored under a **peppered HMAC** —
   `signer.derivedKey(kid, label)` with a label of its own — or not stored
   at all. SHA-256 of an e-mail, truncated or not, is a reversible pseudonym,
   not a hash.
3. **HMAC under a derived key is reserved for derivations**, where the
   server must produce a value from a secret it does not store: the refresh
   successor (`refreshSuccessorLabel = "iterion/auth/refresh-successor/v1"`)
   is the model — HMAC-SHA512 over the rotation nonce and the presented
   token, under `derivedKey(kid, label)`, with domain separation by label and
   key id. Not a storage hash.
4. **Constant time where there is a compare.** PAT verification runs
   `subtle.ConstantTimeCompare` on the two digests after the `token_hash`
   lookup; webhook and config-share after fetching the row by its id; the
   successor check runs `hmac.Equal`. Refresh, reset and invitation resolve
   the row by index equality and compare nothing afterwards — correct for a
   256-bit digest (a timing side-channel on the index reveals nothing about
   the token), and a compare added later takes the constant-time form.
5. **Entropy is the precondition, so it is the thing to check.** A new token
   that hashes through `HashRefreshToken` draws at least 16 bytes of
   `crypto/rand`; a token below that bound is not a bearer token and takes
   the derivation path or a sealed secret ([ADR-146](146-secret-boundary-v2-grants-and-sinks.md)).

## Options considered

- **(a) Keep the digest, fix the prose, record the doctrine — chosen.** No
  behaviour change, no migration, nothing a catalog bot or a workflow can
  feel. The residual risk is a reviewer who expects "salted" by reflex; this
  record carries the argument.
- **(b) Peppered HMAC-SHA256 under the signer's key, dual-read migration,
  key rotation.** About five days: six collections with a unique
  `token_hash` index, seven packages, eighteen test files, a versioned
  on-disk format with lazy rewrite, the multi-key signer wired end to end so
  a rotation does not revoke every token, a C1xx warning on a legacy row
  still read. It defends only the case where MongoDB leaks without
  `ITERION_JWT_SECRET`, which shares its namespace.
- **(c) (a) now, (b) later.** The cost of (b) with no demonstrated gain and a
  doctrine that must be documented twice.

## Consequences

- [secrets-reference.md](../secrets-reference.md), [webhooks.md](../webhooks.md),
  the `pkg/webhooks` and `pkg/configshare` comments and the doc comment of
  `HashRefreshToken` now state the unsalted digest and point here;
  [docs/adr/076-scoped-config-share-editor.md](076-scoped-config-share-editor.md)
  carries a dated addendum — an erratum — its text untouched. A guard in
  `internal/docsguard` (`TestStoredBearerTokenProseSaysUnsaltedAndTheCodeAgrees`)
  refuses a claim that a stored bearer token is hashed with a salt, in the
  files that describe one, and reads the digest back from the function — so
  neither the prose nor the code can drift away from the other unnoticed.
- The two diagrams that already read "unsalted" are the reference wording.
- The DA (V1.1.1, outside this tree) files PATs under "encrypted at rest";
  the exact statement is "stored as a digest, shown once". It is corrected
  in the next DA version, not here.

## Out of scope — named, not decided

- **Single-use tickets** (desktop SSO ticket, WebSocket ticket) and the
  **per-run MCP and forge-publish grants** are short-lived bearer values
  minted by `storekit.NewTicket` / `pkg/hostmcp`. The desktop SSO store keys
  by the digest; the forge-publish Valkey store writes by the digest and
  still reads the legacy plaintext key until its bridge window closes; the
  WebSocket ticket store keys by the plaintext (60-second TTL), and the
  in-memory MCP and forge-publish registries index the raw token (no at-rest
  copy). Whether the WebSocket ticket aligns on the digest is its own small
  change; this record only observes that the digest is the posture its
  sibling already takes.
- A **pepper** for enumerable identifiers (audit subject of a failed login,
  for one) is decided by the record that introduces such a field, under
  clause 2; the label it derives under is its own.
