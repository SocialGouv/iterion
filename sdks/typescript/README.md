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
import { IterionClient } from "@iterion/sdk";

const iterion = new IterionClient({ storeDir: ".iterion" });

const result = await iterion.run("examples/clarify/main.bot", {
  vars: {
    transcript: "User A: ship it. User B: ship what?",
    latest_message: "User B: ship what?",
    thread_id: "thread-1",
  },
  logLevel: "info",
});

// By default `run()` throws on `status: "failed"` (see throwOn below).
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
  file: "bots/whats-next/main.bot",
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

The SDK is versioned independently (`0.1.x` at the time of this checkout) and
tracks the CLI JSON/persisted-format contracts in the same Iterion repository.
Use matching SDK and CLI releases; both surfaces remain experimental and may
change before a stable compatibility policy is declared.

## License

MIT.
