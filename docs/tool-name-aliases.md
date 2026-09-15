# Claw tool-name aliases

**Unreleased: the 3.146.0 floor in this change is provisional.** Before merging,
set `bundle.ToolAliasesSince` to the first release that contains this resolver;
other changes may have advanced the next release number. Re-run the compatibility
probe with the finalized floor. Do not publish an alias-using bundle before the
runner image carrying that release has been deployed.

The number is always the next *unreleased* version. It read 3.144.0 while that
release was still ahead; 3.144.0, 3.144.1 and 3.145.0 have since shipped without
this resolver, and a floor naming any of them would admit a runner that treats
`Read` as MCP only — the exact failure this gate exists to prevent.

Claw accepts the exact spellings `Read`, `Bash`, and `Grep` as `read_file`, `bash`,
and `grep` when the bundle declares `requires.iterion` at or above the release
containing this feature. Canonical names continue to work in a plain `.bot` or
on older engines. This is engine-version-dependent behavior in both DSL profiles;
it is not a profile-1 lowering that an older reader can reproduce.

```yaml
# manifest.yaml — use the finalized first release, currently provisional:
requires:
  iterion: ">= 3.146.0"
```

```iterion
agent inspect:
  backend: claw
  model: "openai/gpt-5.6-sol"
  tools: [Read, Bash, Grep]
  tool_policy: [Read, Bash, Grep]
```

The entire resolution order matters:

1. A registered exact name wins.
2. Qualified MCP names (`mcp.server.Read`, `mcp__server__Read`) retain their
   existing meaning. They are never builtin aliases.
3. A bare name matching one MCP tool resolves to that tool. Matching two MCP
   tools is an ambiguity error, even if a builtin alias has the same spelling.
4. Only a remaining unresolved exact alias uses its canonical builtin.

The compiler leaves the source names intact. It cannot see the project `.mcp.json`
and plugin servers that are connected after compilation. C135 therefore warns
with the canonical spelling and manifest remedy; the runtime resolves against the
complete connected catalog. Without the declared floor it keeps the old resolver
and refuses an alias that would otherwise select a builtin before calling a model.
A unique MCP `Read` still works without an alias opt-in, just as before.

This applies to agent/judge `tools:`, the same list when a fallback route selects
Claw, and Verified Action `recovery.agent_tools`. Workflow/node `tool_policy`
patterns use the same registry precedence when aliases are enabled: a policy
`Read` selecting an MCP grants that MCP, never `read_file`. An ambiguous alias in
a matched policy denies the call, including beside `*`. The model receives one
canonical tool schema even if both alias and canonical names were listed.

`capabilities:` contains host rights such as `board.read` and `runs.read`, not tool
names. `capabilities: [Read]` remains malformed (C081); aliases do not grant host
rights. CLI backends retain their existing tool and permission semantics.

Each engine derives this opt-in from its own bundle on Run, Resume and branch
dispatch. A child without a sufficient floor does not inherit an opted-in parent's
setting. The existing bundle admission gates reject an older engine; the queue
schema and AST retain their original shape. Rollback therefore requires reverting
the alias-using bundle as well, or the older image will correctly refuse it.

## Verification

The local regression test `TestToolAliasesThroughRealEngineAndClawTools` compiles
a workflow, runs the real engine and Claw executor, and calls real file, search and
shell tools against a temporary proof file. Only the provider's choice of calls is
scripted. Missing/insufficient floors produce zero provider calls. Separate tests
cover zero/one/two MCP suffixes, exact builtin priority, qualified names, policy
isolation and duplicate schemas. The same real tools are exercised through a
Claw fallback, a rung-4 recovery agent satisfying a filesystem postcondition,
and a fresh engine resuming a failed run.

On 2026-09-14 the [compatibility probe](../scripts/compat/tool-aliases/README.md)
passed with a real new `SubmitLaunch` producer and the unchanged 3.143.0 runner at
`882c76afbb`: the old reader accepted queue schema 17, the serialized AST and
the immutable bundle snapshot emitted by the producer (no fallback bundle store),
then `executeRun` refused the bundle as `failed/BOT_REQUIRES_NEWER_ENGINE` before
executing tools or models. This is evidence of refusal under version skew, not an
assertion that release 3.144.0 already contains the feature.
