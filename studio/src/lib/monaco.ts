// Self-hosted Monaco. Import `Editor` / `DiffEditor` from HERE, never from
// `@monaco-editor/react` directly.
//
// Left to itself, @monaco-editor/react fetches the editor at runtime from
// `https://cdn.jsdelivr.net/npm/monaco-editor@<v>/min/vs`. That put executable
// third-party code in the surface that edits LLM keys, OAuth forfaits and
// forge tokens, leaked the operator's IP to the CDN on every editor open, and
// pinned a runtime version nothing in this repo declares. It also contradicted
// the same no-CDN rule the fonts already follow (see main.tsx), and made a
// `script-src 'self'` CSP impossible.
//
// `loader.config({ monaco })` hands the library the instance the bundler
// resolved, so nothing is fetched at runtime. Everything monaco lands in the
// `monaco` chunk group already declared in vite.config.ts.
//
// Deliberately NO `MonacoEnvironment`: since 0.56 each language service
// carries its own `createWorker: () => new Worker(new URL('x.worker.js',
// import.meta.url), { type: 'module' })`, which the bundler resolves and emits
// from our own origin — so workers are self-hosted too, and `worker-src 'self'
// blob:` covers them. Defining MonacoEnvironment.getWorker would OVERRIDE that
// (see esm/vs/internal/common/workers.js: the environment hook is consulted
// first and the built-in factory only runs when it is absent), replacing a
// correct default with a hand-kept label→worker map.
import * as monaco from "monaco-editor";
import Editor, { DiffEditor, loader, type Monaco } from "@monaco-editor/react";

loader.config({ monaco });

export { Editor, DiffEditor, monaco };
export type { Monaco };
export default Editor;
