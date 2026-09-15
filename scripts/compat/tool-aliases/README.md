# Tool-alias compatibility probe

These are temporary test harnesses, not alternate implementations of the reader.
`publisher_test.go.txt` calls the **new** `Publisher.SubmitLaunch`; its captured
RunMessage is decoded and executed by the **old** `Runner.executeRun` after copying
`runner_test.go.txt` to that checkout. Neither harness calls a model or a forge.

Use an isolated, unchanged old worktree (the recorded run used commit
`882c76afbb940a002d1a3edfcbdc97a7f02c6221`, release 3.143.0). Copy the first file to
`pkg/server/cloudpublisher/tool_aliases_compat_probe_test.go` in the new checkout,
and the second to `pkg/runner/tool_aliases_compat_probe_test.go` in the old checkout.
Do not replace an existing file. In each checkout run, in order:

```sh
# New checkout, with an existing directory outside either Git checkout:
ITERION_ALIAS_COMPAT_DIR=/tmp/tool-alias-compat devbox run -- go test \
  ./pkg/server/cloudpublisher -run '^TestAliasCompatibilityExport$' -count=1 -v
# Old checkout, reading the SAME captured bytes:
ITERION_ALIAS_COMPAT_DIR=/tmp/tool-alias-compat devbox run -- go test \
  ./pkg/runner -run '^TestAliasCompatibilityOldRunner$' -count=1 -v
```

Remove only the two copied test files afterward. The captured `message.json` and
`main.bot` contain fixture data only and may be kept as evidence. The producer
embeds a real immutable bundle snapshot whose manifest uses
`bundle.ToolAliasesSince` (currently the **unset 9999.0.0 sentinel**, so the
probe exercises a floor no runner satisfies — set the real release before
reading anything into a green result). The old reader has
no bundle store fallback: the manifest it refuses must come from those published
bytes. The old build is pinned separately in its harness.

The source deliberately declares MCP inheritance: a profile-1 reader already
accepts these bare names as potential MCP shorthands. Thus the test reaches the
existing engine-floor gate instead of succeeding trivially because C135 refused
`Bash` before the bundle could load. Required result: compatible schema and AST,
then `failed/BOT_REQUIRES_NEWER_ENGINE`, before any tool/model execution.
