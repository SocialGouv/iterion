# ADR-041: Ultracode is a mode, not a wire value, gated to Opus 4.8

- **Status**: Accepted
- **Date**: 2026-06-22
- **Authors**: Adry
- **Code**: [pkg/dsl/ir/validate.go](../../pkg/dsl/ir/validate.go), [pkg/backend/model/effort.go](../../pkg/backend/model/effort.go)

## Context

The DSL exposes `reasoning_effort` as a user-facing control. `ultracode` is the top orchestration dial, but Anthropic's API accepts only provider-defined effort values up to `xhigh`/`max`; `ultracode` is not a native wire value.

The mode also carries a behavioural meaning beyond model compute: xhigh reasoning plus standing permission for workflow orchestration. That makes it a product/runtime mode rather than just another provider parameter.

## Decision

The validator accepts `reasoning_effort: ultracode` as a valid DSL value. It warns with diagnostic `C089` when the selected model does not carry the orchestration prerogative, because it degrades elsewhere.

> **The model set is superseded.** As accepted, this ADR gated the warning on `claude-opus-4-8` alone — the only model carrying the orchestration half at the time. The **Claude 5 family** (`claude-opus-5`, `claude-fable-5-1`) carries it too, so the gate admits it as well, and the predicate now lives in one exported function, [`ir.ModelSupportsUltracode`](../../pkg/dsl/ir/validate_nodes.go), shared with the studio's `/api/effort-capabilities` endpoint — previously a second, separate copy that disagreed with the compiler. The *decision* recorded here — ultracode is a mode remapped to `xhigh` on the wire, gated by an advisory warning rather than an error — is unchanged. Current contract: [docs/ultracode.md](../ultracode.md).

The backend effort mapping in [`pkg/backend/model/effort.go`](../../pkg/backend/model/effort.go) treats `ultracode` as the highest internal rank but maps it to `xhigh` before provider-wire coercion. This prevents an unsupported literal `ultracode` from being sent to APIs that do not accept it.

The runtime can still inject the orchestration prerogative through system-prompt behaviour while the provider receives a supported reasoning effort.

## Trade-offs

| Dimension | DSL mode remapped to `xhigh` | First-class provider wire value |
|---|---|---|
| API compatibility | Never sends unsupported `ultracode` to providers. | Would produce provider errors until APIs support it. |
| Product semantics | Captures orchestration permission separately from wire effort. | Treats orchestration as if it were only model compute. |
| User feedback | Warns outside `claude-opus-4-8` with `C089`. | Either rejects broadly or silently misrepresents support. |
| Internal ordering | Can rank ultracode as top mode. | Depends on provider effort taxonomy. |

The honest concession is that `ultracode` can degrade to plain `xhigh` on models where the orchestration semantics are not reliable.

## Alternatives considered

### 1. Send `ultracode` as a first-class API effort

The backend could have treated `ultracode` like `low`, `high`, or `xhigh` and passed it through to the provider.

**Rejected because**: Anthropic does not accept `ultracode` as a wire value; it is product orchestration permission, not a provider model parameter.

### 2. Reject `ultracode` everywhere except Opus 4.8

The validator could have made non-Opus-4.8 use a hard error rather than a warning.

**Rejected because**: the underlying compute can still degrade safely to `xhigh`; a warning communicates the semantic downgrade without unnecessarily breaking workflows.

## Consequences

- **Provider requests stay valid.** Runtime effort coercion collapses `ultracode` to `xhigh` before the wire.
- **The DSL can express the top orchestration mode.** Users have a stable mode name even before providers expose a matching parameter.
- **Model gating is visible.** Diagnostic `C089` warns when the mode is used outside `claude-opus-4-8`.
- **Internal ranking preserves intent.** Scheduling/coercion code can still treat `ultracode` as the highest requested mode.
- **Rechallenge if providers add native support.** If a provider introduces a real `ultracode` effort, the remap and prompt-injection path should be removed or narrowed.
