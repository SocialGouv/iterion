# @iterion/sdk

TypeScript SDK for the [iterion](https://github.com/SocialGouv/iterion)
workflow orchestration engine. The SDK is a thin, typed wrapper around
the `iterion` CLI binary — every method shells out and parses the
`--json` output into typed result objects.

## Install

```bash
pnpm add @iterion/sdk
# or
npm install @iterion/sdk
```

You also need the `iterion` binary on your machine. Either install it
([release page](https://github.com/SocialGouv/iterion/releases)) and
make sure it is on `PATH`, or set the `ITERION_BIN` environment variable
to its absolute path.

## Quickstart

```ts
import { IterionClient, IterionRuntimeError } from "@iterion/sdk";

const iterion = new IterionClient({ storeDir: ".iterion" });

try {
  const result = await iterion.run("examples/my_workflow.iter", {
    vars: { repo: "my-repo" },
    logLevel: "info",
  });

  // By default `run()` THROWS on `status: "failed"` (see throwOn below).
  // The remaining terminal statuses are returned for the caller to branch on.
  switch (result.status) {
    case "finished":
      console.log("done", result.run_id);
      break;
    case "paused_waiting_human":
      console.log("waiting for", result.questions);
      break;
    case "cancelled":
      console.warn("cancelled");
      break;
  }
} catch (err) {
  if (err instanceof IterionRuntimeError) {
    console.error(`workflow failed [${err.code}]:`, err.message);
  } else {
    throw err;
  }
}
```

To opt out of the default throw-on-failed behaviour and inspect the
`failed` result directly, pass `{ throwOn: [] }` (or any non-empty
subset of `"failed" | "cancelled" | "paused_waiting_human"`).

### Stream events

```ts
const ac = new AbortController();
for await (const evt of iterion.events(result.run_id, { follow: true, signal: ac.signal })) {
  console.log(evt.type, evt.node_id ?? "");
}
```

### Resume a paused run

```ts
const resumed = await iterion.resume({
  runId: result.run_id,
  file: "examples/my_workflow.iter",
  answers: { approve: true, notes: "looks good" },
});
```

## API

The public surface is documented in source under `src/`:

- `IterionClient` — façade with `run`, `resume`, `inspect`, `validate`,
  `diagram`, `report`, `init`, `version`, `events`, plus store helpers
  `loadRun`, `loadInteraction`, `loadArtifact`, `listRuns`.
- `IterionRuntimeError`, `IterionInvocationError`,
  `IterionBinaryNotFoundError`, `IterionRunPausedSignal` —
  structured errors.
- `tailEvents`, `resolveBinary`, `detectPlatform` — exported helpers
  for advanced use.

## Status

This package targets iterion `v0.2.x`. APIs may change without notice
while iterion itself is experimental.

## License

MIT.
