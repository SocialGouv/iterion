---
name: canonicalization
description: Neutralise what is volatile in a captured response without erasing what is business signal — the line between the two, and the traps on each side.
---

# Canonicalisation

A raw response is not comparable to itself. Session identifiers, timestamps, CSRF tokens and
serialisation order all differ between two captures of an unchanged application. Canonicalisation
removes that noise.

It is also the easiest place to destroy the entire exercise. Every rule you add is a class of
change the net can no longer see.

> Canonicalise the **volatile**. Never the **business**.

| Volatile — neutralise | Business — leave alone |
|---|---|
| session ids, CSRF tokens, nonces, request ids | the **order of a list** |
| rendered timestamps and dates | the **number of items** returned |
| generated UUIDs, password hashes | pagination totals |
| dictionary/object key order | field values, however boring |
| the host and port of the instance | status codes |
| `Date`, `Set-Cookie`, `Expires` headers | `Content-Type`, `Location` path |

## The canonicaliser needs its own tests, and they are not optional

Everything else in the net is judged by something. Comparators are judged by the
mutation counter-test. The gate is judged by its own conjunction. The seal is
judged by the fingerprint refusal. The canonicaliser is judged by **nothing** —
no mutant exercises it, because a mutant that a rule swallows simply reads as
undetected, and one that a rule leaves alone reads as detected. It is invisible
either way.

That matters because the mistake here is **asymmetric**:

| mistake | what the net does | who notices |
|---|---|---|
| canonicalise too little | goes unstable, so RED | everyone, immediately |
| canonicalise too much | goes BLIND, so GREEN | nobody, ever |

A rule that is too wide does not fail. It **silences**. And it silences exactly
the regression it was meant to let through, because the reason a rule gets
widened is almost always that something legitimate kept moving.

So write tests, and write them **in both directions**. The "volatile is
neutralised" cases are easy and nearly worthless; the "business survives" cases
are the file's reason to exist:

- a field that merely *looks* like a timestamp must survive — match by exact key
  name, never by value shape, and let a test prove it
- the same value rendered as displayed text must stay compared while its
  volatile serialised twin is neutralised — **match the serialisation, not the
  concept**
- array order must survive, because a datastore migration produces precisely
  that kind of reordering and it is the signal, not the noise

Then **falsify them**: write the over-canonicalisation you fear, watch the right
test go red, and restore. A test suite on a canonicaliser that has never been
seen to fail is decoration.

Finally, make them **blocking in the emitted runner**, ahead of anything that
starts the application. They cost milliseconds, and a broken canonicaliser must
not be able to produce a verdict at all — not a red one, not a green one.

## The trap that matters most

**Never sort collections "to stabilise them."**

Ordering is the single most likely thing to shift when a database engine changes: collation
differs, tie-breaking among equal-ranking rows differs, default index ordering differs. That
shift is precisely what a migration net exists to surface. A canonicaliser that sorts lists is
not stabilising the capture — it is deleting the finding.

If a list genuinely comes back in a different order on two captures of an *unchanged*
application, you have found non-determinism in the application, not noise to paper over. Fix it
at the source (an explicit `ORDER BY`, a deterministic tie-break) or record it as a known
instability. Do not sort it away.

The `order_flip` mutant ([[oracle-mutation]]) exists to keep you honest here, and it belongs in
the held-out set: it changes ordering **without changing a single displayed value**, so a
sorting canonicaliser makes it perfectly invisible.

## Symmetric trap

Over-scrubbing is the mirror image. A regular expression broad enough to swallow every date will
also swallow an identifier that merely looks like one. Anchor patterns tightly: match a full
timestamp, not any run of digits and separators. A `field_drop` mutant catches a canonicaliser
that has become so lossy it erases whole fields along with the volatile ones.

## The contract

`.golden-master/canon/rules.py` exposes one function:

```python
def canonicalize(entry, status, headers, body):
    """entry: the corpus entry (dict) · status: int · headers: dict · body: bytes
    Returns the canonical text compared against the reference."""
```

Practical shape:

1. Start the output with the **status line**. A comparator that reads only the body cannot see an
   authorisation regression — the `status_change` mutant exists for exactly that.
2. Emit only **contract-bearing headers**. Strip the scheme and host from `Location`; the path is
   the contract, the port is an accident of the fixture.
3. Decode the body. For JSON, re-serialise with **sorted keys** — object key order follows the
   serialiser's reflection order and is not signal. Sorting *keys* is safe; sorting *arrays* is
   the trap above.
4. Apply the volatile substitutions, each replacing with a stable marker (`<TS>`, `<CSRF>`) rather
   than deleting — a marker keeps the shape of the document visible and a deletion does not.

## Document each rule by its cause

Every substitution answers "what made this differ between two identical runs?". Write that cause
next to the rule. A rule without a cause is a rule nobody can safely remove later, and it will
outlive the reason it was added.

Some causes worth recognising, because they look like regressions and are not:

- **object key order** follows the serialiser's field-reflection order and shifts on a library
  upgrade
- **a new key appearing in a paginated envelope** is often a framework version adding a field
- **timezone offset formatting** (`+0000` versus `+00:00`) changes with serialiser versions
- **auto-generated element ids** shift when a UI component library is upgraded

Each is genuinely volatile. Each must still be pinned narrowly enough that a real change in the
same place is not swallowed with it.

## Order of operations

Some rules must run before the document is parsed. A malformed or self-closing tag can make an
HTML parser mis-nest a whole subtree, so removing that element *after* parsing may take far more
with it than intended. When a rule is order-sensitive, say so in a comment — the next reader will
otherwise reorder it for tidiness.

## Stop when it is stable

The harness captures twice on one boot and once after a restart, then requires all three to be
identical. Do not start reading mutation figures before that passes: a score computed on a
drifting capture measures the drift, not the oracle.
