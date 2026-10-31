# Facade substitution — the served model is not the label

Read it when a `zai`/`moonshot` route answers with GLM/Kimi under a
claude/gpt label, or when a set base URL looks like an exemption and the
refusal does not fire.

An Anthropic-shaped facade (z.ai, Moonshot) answers ANY model id with the
one model its account aliases it to: the call succeeds, declared and
effective ids agree on the wire, and the run's record names a model that
never saw the prompt. The claude_code delegate refuses such a route BEFORE
the spawn (#2772), typed `unavailable`, so a declared `fallbacks:` route
whose `on:` names it fires by the chain's own machinery
([when a route is taken](../../backends.md#when-a-route-is-taken)); the
claw registry raises the same refusal from its z.ai synthesis and both its
moonshot factories ([facade_substitution.go](../../../pkg/backend/delegate/facade_substitution.go),
[registry.go](../../../pkg/backend/model/registry.go)).

**One claim predicate, both backends.** The verdict honours only an
operator endpoint that DIFFERS from the vendor's own
(`delegate.OperatorEndpointClaim`): unset, or the vendor's documented base
URL however cosmetically respelled (case, spacing, trailing slash), is the
facade's own wiring and the refusal applies — on the exact env z.ai
documents (`ZAI_API_KEY` + `ANTHROPIC_BASE_URL`=z.ai) the delegate refuses
a claude id and claw refuses it too. An endpoint that genuinely differs
(the enterprise proxy) stays the operator's claim and passes.

**The record names the serving route.** Whichever route served, the run
record carries provider AND model bound together — `NodesServed.provider`
in the store, `_provider` beside `_model`/`_served_by` in the node output,
re-stamped across a schema re-ask like them — so the run table and a
deterministic gate read whose endpoint answered, not only the label it
answered under ([what a fall-through leaves behind](../../backends.md#what-a-fall-through-leaves-behind)).
