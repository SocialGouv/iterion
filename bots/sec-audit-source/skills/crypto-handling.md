---
name: crypto-handling
description: |
  Hard-stop policy for crypto findings. Seki proposes the patch for
  visibility but DOES NOT auto-apply, regardless of ladder result.
  Flags `risk_flag: touches_crypto_primitive`, downgrades verdict
  to `uncertain`, and routes to human review.
---

# crypto-handling — hard-stop, no auto-patch

Crypto code is one of the few places where a "correct-looking,
ladder-passing" patch can be wrong in ways that are catastrophic and
silent. This skill states the policy that `[[patch]]` enforces.

## The rule

Seki **MUST NOT auto-patch** a finding when ANY of the following
hold:

1. `finding_type == "crypto"` per `[[finding-taxonomy]]`.
2. The patched file path matches any of:
   - `**/crypto/**`, `**/cryptography/**`
   - `**/cipher*/**`, `**/aead/**`, `**/kdf/**`, `**/hkdf/**`
   - `**/keyexchange/**`, `**/handshake/**`, `**/keystore/**`
   - `**/jwt/**`, `**/jws/**`, `**/jwe/**`
   - `**/signature*/**`, `**/verify*/**`, `**/signing/**`
   - `**/keys/**`, `**/keymgmt/**`, `**/kms/**`, `**/hsm/**`
   - `**/rng/**`, `**/random/**` (RNG primitives)
   - `**/tls/**`, `**/pki/**`, `**/x509/**`, `**/certs/**`
3. The patch diff touches a constant-time-sensitive helper
   (`subtle.ConstantTimeCompare`, `crypto.timingSafeEqual`,
   custom HMAC compare loops).

When matched, `[[patch]]`:

1. Generates the candidate diff for visibility (author phase runs).
2. **Skips** the verification ladder beyond build (no
   reproduce/regress/re-attack — the oracles do not catch
   constant-time leaks or algorithmic mis-implementations).
3. Sets `risk_flag: touches_crypto_primitive`.
4. Forces `verdict: uncertain` (never `ladder_passed`, even if
   the build tier succeeded).
5. Creates the kanban issue in `state: review` with labels
   `human-review`, `risk:crypto`, in addition to the standard
   `severity:*`, `type:crypto`, `source:sec-audit-source`.

## Why — high-assurance code, subtle breakage

Crypto bugs are different from the rest of the taxonomy:

- **Silent failure mode.** A weakened MAC, a non-constant-time
  compare, a re-used IV, or a downgraded RNG produces correct
  outputs on every test in the regression suite — the failure is
  observable only by an attacker with carefully chosen inputs or a
  timing channel.
- **Tier-3 re-attack can't see it.** Seki's per-category re-attack
  oracles (cf. `[[reattack-oracles]]`) re-run scanners. Scanners
  catch *patterns* (`rand.Read` vs `math/rand.Read`); they do not
  catch the wrong nonce length, an off-by-one in scalar arithmetic,
  or a hash-then-MAC vs encrypt-then-MAC mistake.
- **Constant-time concerns.** A `==` comparison that "looks the
  same" as `crypto/subtle.ConstantTimeCompare` is not the same.
  Compilers will sometimes optimize a hand-written constant-time
  loop into a non-constant-time one across versions.
- **Spec compliance, not just code correctness.** Ring signatures,
  ratchets, and KDFs are correct only against the *protocol spec*.
  An LLM diff that compiles and re-passes the unit test can be
  protocol-correct on the patched call site and silently broken on
  the others (the protocol assumes ALL call sites maintain an
  invariant; the patch may have updated only one).
- **Forward-secrecy and key-rotation gotchas.** A change that
  passes today can break decryption of past-encrypted blobs after
  the next deploy.

## What the candidate diff is for

The author-phase diff is still useful — it gives the human
reviewer:

- A concrete starting point on what to change.
- A `<rationale>` block citing the root cause file:line.
- A `<variants_checked>` block naming sibling call sites that need
  parallel fixes.
- The reproducer test recipe shape (informational; see
  `[[reproducer-go]]` and `[[reproducer-ts]]` for the per-finding
  test shapes).

The reviewer is expected to discard, rewrite, or extend it. The
diff is a draft, not a proposal Seki stands behind.

## Escalation path

The kanban issue carries:

```yaml
title:  "CRYPTO HUMAN REVIEW: <short>"
state:  review
labels:
  - severity:<level>
  - type:crypto
  - source:sec-audit-source
  - human-review
  - risk:crypto
body: |
  ## Why this is in review

  Seki detected a crypto-touching change and is NOT auto-patching.
  See `crypto-handling.md` for policy.

  ## Candidate diff (DRAFT)

  Path: `.sec-audit/patches/<id>/patch.diff`

  ## Reviewer checklist

  - [ ] Diff matches the actual protocol spec (cite RFC / paper).
  - [ ] Constant-time properties preserved (no `==` on secret
        comparisons, no early returns, no length-leaking branches).
  - [ ] RNG source is `crypto/rand` (Go) /
        `crypto.randomBytes` / `crypto.getRandomValues` (TS/JS) —
        never the math-package fallback.
  - [ ] All sibling call sites listed in <variants_checked> are
        updated consistently.
  - [ ] Forward-secrecy / key-rotation behavior unchanged or
        documented.
  - [ ] Reviewed against `.sec-audit/context.md` threat
        model section 4.

  ## Original finding

  - finding_type: crypto
  - file: <path>:<line>
  - scanner: <id>
  - description: <triage rationale>
```

The issue moves through `review -> ready` only after a human
flips it manually (Seki has no `board.move` capability on crypto
issues). `done` requires a human commit referencing the issue id.

## If the matcher mis-fires

Sometimes a path matches the regex but the finding is genuinely
not crypto (a file named `crypto_test.go` testing an unrelated
struct, a `tls` package that only formats logs). In that case the
human reviewer:

1. Adds an entry to `.sec-audit/fp-known.yaml` (see
   `[[fp-memory]]`) so future runs don't re-surface it.
2. Closes the kanban issue with `won't-fix`.

The hard-stop stays in place. Do not soften it for "obvious"
non-crypto cases — the policy is precisely calibrated to err on
the side of human review, because the failure mode of getting it
wrong is silent and catastrophic.

## See also

- `[[patch]]` — the verification ladder this policy short-circuits.
- `[[finding-taxonomy]]` — `crypto` category definition.
- `[[reviewer-isolation]]` — the reviewer contract still applies
  to the diff when a human looks at it.
- `[[reattack-oracles]]` — why scanner re-attack alone is
  insufficient for crypto.
- `[[fp-memory]]` — appending mis-fires to `fp-known.yaml`.
- `[[iterion-board]]` — kanban labels and state transitions.
