# Codemode concurrency

`call(name, input)` still runs one tool and throws on ordinary tool failure.
`call.parallel(entries)` submits up to four `{name, input}` entries and joins
before returning to JavaScript. It does not return a promise. An empty batch
returns `[]`; an invalid or oversized batch fails before it is sent.

```js
call.parallel([
  {name: "glob", input: {pattern: "docs/**/*.md"}},
  {name: "glob", input: {pattern: "pkg/**/*.go"}},
]).map(result => ({ok: !result.isError, text: result.text}));
```

Each entry returns `{tool, status, text, isError, truncated, remaining}` in
input order. `status` is `ok`, `error`, or `budget`. An ordinary error does
not cancel the other entries. The parent reserves the admissible prefix
before running any member; the suffix reports `budget` without executing.
Every result gives the same post-admission `remaining`. A dispatcher abort
or a broken tool channel ends the script. Cancellation cannot undo effects
an already-started tool performed.

`concurrent_tool_calls` inherits like the other codemode limits. Its default
is 1 (serial), and the platform ceiling is 4. This limit bounds work in flight,
not the batch's cardinality. A tool without an audited concurrency capability
runs exclusively, between groups of eligible calls. Every call still goes
through the node's own dispatcher with its existing permissions, hooks and
executable scope. The child does not choose eligibility. The scoped `glob`
and `workspace_grep` registrations are eligible when the dispatch path is
also audited; custom hooks or materializers keep the batch serial. MCP
calls to the same client share one cancellable slot across tool names until
the local SDK call returns. A remote handler may still be cleaning up after
cancellation. The script host joins started handlers before returning;
handlers must cooperate with cancellation for shutdown to be prompt.

The launcher keeps reading events and session captures while forwarded tools
run. Forwarded tools and operator questions share a serial lane; its
admission and reply backlogs are bounded separately. This does not
change tool placement or establish sandbox-crossing support for codemode.
Queue schema v34 protects delivery to the cloud runner, not the runner in a
separate sandbox image: a pre-concurrency image can ignore the new bound.

A resume with no bounds replays the saved override. Restating any bound
replaces that entire override block; restate the width too if it must stay
pinned.
