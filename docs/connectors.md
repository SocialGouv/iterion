# Connector packages

A **connector package** is the catalog's unit of third-party access: a vendor's
API reduced to flat, typed operations a workflow calls with no model in the
loop. This page is the package *format* — which files a package is made of,
which half of it is generated, and what the authored overlay is for.

The surfaces around the format live elsewhere. The commands that build and
check a package are [`iterion connectors`](cli-reference.md#iterion-connectors);
the credential that authenticates a call is
[`iterion connections`](cli-reference.md#iterion-connections); the node that
makes one is [the `action:` recipe](dsl.md#tool), which also covers the guarded
dialer and the operator/project catalog tiers a package is resolved from; the
reasoning behind all of it is [ADR-098](adr/098-connector-catalog.md).

The shipped example is [`connectors/forgejo/`](../connectors/forgejo) — 503
operations across ten domain files, generated from Forgejo's Swagger 2.0
description.

## The files

| Path | Half | Carries |
|---|---|---|
| `connector.yaml` | generated | `schema_version`, `id`, `display_name`, `version`, the `provenance` block (`spec_format`, `spec_version`, `spec_license`, `redistributable`, `generated_at`, `generated_by`), `base_url` (`path_prefix`, `operator_supplied`) and the derived `auth:` schemes |
| `ops/<domain>.yaml` | generated | one file per vendor tag — the file's `domain:`. Each operation is an id, an `http:` method and templated path, flat typed params, `results:` by status naming a shared schema, and its `effect` / `deterministic` marking |
| `schemas.yaml` | generated | the schemas operations name by `schema_ref`. They dominate a large package, so they are shared rather than inlined per operation |
| `responses.json` | generated, package format v2 only | the explicit response contracts `iterion connectors gen --validate-responses` derives, a separate authority from the descriptive `schemas.yaml` |
| `overlay.yaml` | **authored** | everything the vendor's description cannot state — below |
| `identity.lock.yaml` | maintained by `gen` | operation and auth id stability across regeneration ([connector-identities.md](connector-identities.md)) |

Everything generated is disposable: regenerate and get the same bytes. The
generated files never contain the applied overlay — `iterion connectors
validate` applies it to a copy, so what it reports on is the package a launch
actually resolves.

## What the overlay carries

`overlay.yaml` ([`pkg/connector/overlay`](../pkg/connector/overlay/overlay.go))
has its own `schema_version`, deliberately independent of the package's:
borrowing the package constant meant a package-format bump silently widened
what an overlay may declare. Its `connector:` field must match the package id,
checked rather than assumed — an overlay applied to the wrong package renames
operations that do not exist and silently corrects nothing.

Connector-wide keys:

| Key | Effect |
|---|---|
| `display_name`, `description` | replace the vendor's own wording when it is a title rather than a label |
| `auth` | **replaces** the derived schemes rather than merging into them |
| `base_url` | `default`, `path_prefix`, `operator_supplied` — each a pointer, so setting one leaves the others alone |
| `default_security` | replaces the derived root-level requirements |
| `outcome` | the connector-wide success/failure policy |
| `maturity` | the package's readiness floor |
| `drop` | removes operations by derived id — a deprecated endpoint, one dangerous to expose, one the catalog should not carry |
| `operations` | per-operation corrections, keyed by derived id |

Replacement rather than merge is the whole point of `auth:`. The case that
matters is a description that declares the wrong schemes, or none at all, and a
merge makes "state the truth" impossible. Forgejo declares five security
definitions and only two are credentials a connection can hold: `Sudo` / `sudo`
are an admin impersonation *modifier* and `X-FORGEJO-OTP` is a second factor
used alongside BasicAuth, so offering any of the three in a connection wizard
would ask an operator to authenticate with something that is not an identity.
The `token ` `value_prefix` is restated in the overlay even though it *was*
derived, out of the description's own prose — it is one vendor docs edit away
from disappearing, and without it every call answers `401`, which reads as a
bad credential rather than a missing word.

Per-operation keys, under `operations:` and keyed by the **derived** id:

| Key | Effect |
|---|---|
| `id` | the public name after generation |
| `summary`, `description` | for a curated MCP tool this is what an agent reads to choose it, so it is worth writing |
| `mcp` | puts the operation in the facade's curated tool set |
| `deterministic` | can only be turned **off**. An overlay may declare an endpoint not certifiable on the node path; it may not declare that one the generator refused is fine, because the generator's refusals are structural |
| `effect` | corrects a misread — a `POST` that searches, a `PUT` that creates |
| `maturity` | the operation's own readiness, clamped by the package's |
| `pagination` | how the collection walks |
| `outcome` | overrides the connector-wide policy for this operation |
| `idempotency_key_param` | the parameter that makes a retry safe |
| `security` | replaces the derived requirements for this operation |
| `params` | per-parameter corrections, keyed by derived key |

Per parameter, under an operation's `params:`: `key` renames the public key
without touching the wire name — a vendor's `q` becomes `query` for a `.bot`
author and the request is unchanged; `description` and `required` correct the
description; `secret` marks a value that must never be logged or shown to a
model (Slack declares its credential as an ordinary parameter, so without it a
token would travel through an agent's context); `style` and `explode` correct a
serialization the description got wrong or left implicit.
