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
operator endpoint outside the vendor's published ensemble
(`delegate.OperatorEndpointClaim` over
[secrets.FacadeVendorEndpoints](../../../pkg/secrets/byok.go)): unset, or
any of the vendor's own hosts — the default AND the second published host
of the same wire (Moonshot's `api.moonshot.cn` gateway, Zhipu's
`open.bigmodel.cn` for the z.ai family) — however respelled — is the
facade's own wiring and the refusal applies. A base URL that parses
NOWHERE — a lone slash after the scheme, an empty authority, backslashes,
a control character or a refused escape in the host — is no claim either
(`secrets.EndpointHasHost`): the verdict fails closed, because the WHATWG
parser the claude CLI spawns reads several of those shapes as the vendor's
host. An endpoint that genuinely differs (the enterprise proxy) stays the
operator's claim and passes.

**One fold, both directions.** "Respelled" is
[`secrets.SameEndpoint`](../../../pkg/secrets/endpoint_identity.go), the
routing identity every credential-routing verdict compares through (the
claim here, `AnthropicForfaitWireOK`'s wire pin): case, surrounding
spacing, a trailing slash, the port the scheme implies — read as the
number a dial resolves, so `:0443` folds like `:443` — userinfo, a query,
a fragment, an absolute-FQDN or internationalised host, and the path's
escapes and empty/dot segments all fold: none of them changes where the
call is dialled. The authority never folds: another host, a non-default
port, another scheme, or a host merely CONTAINING the vendor's name
(`api.z.ai.elsewhere`, or the vendor's name worn as userinfo over
another host) is another destination and stays the claim. Whitespace
inside a host is no respelling either — nothing dials such a name, so it
reads as written.

**A blessed host the egress cannot reach is a dead route.** Every
endpoint of the ensemble is a host of the sandbox allowlist and of the
model-detection table (`netproxy.iterionDefaultRules`,
`netproxy.modelHosts`), read from `secrets.FacadeProviders` by the guards
in `pkg/sandbox/netproxy` so a vendor added to the ensemble cannot drift
out of either: missing from the first, a sandboxed node on the blessed
route dies at the CONNECT proxy on an empty payload rather than a
credential error; missing from the second, its conversation is secret-
substituted on the way out.

**The record names the serving route.** Whichever route served, the run
record carries provider AND model bound together — `NodesServed.provider`
in the store, `_provider` beside `_model`/`_served_by` in the node output,
re-stamped across a schema re-ask like them — so the run table and a
deterministic gate read whose endpoint answered, not only the label it
answered under ([what a fall-through leaves behind](../../backends.md#what-a-fall-through-leaves-behind)).
