// The bundled Monaco instance. NOT the import point for components — use
// `./monaco`, which wraps these in React.lazy so 4.2 MB does not land on the
// importer's critical path. Everything here is eager by construction.
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
// MonacoEnvironment.getWorker is REQUIRED, and its map must answer EVERY
// label. Monaco has two worker mechanisms and only one survives the bundler
// unaided:
//
//   - the four LANGUAGE services (json/css/html/ts) each carry a
//     `createWorker: () => new Worker(new URL('x.worker.js', import.meta.url))`
//     that Vite resolves and emits as a local asset;
//   - the EDITOR worker (label `editorWorkerService`) does not. It resolves
//     through `esmModuleLocationBundler` to a 544-byte module — under Vite's
//     4096-byte `assetsInlineLimit`, so it is inlined as a
//     `data:text/javascript` URL, and the inlined text still carries RELATIVE
//     imports, which cannot resolve against a `data:` base.
//
// Leaving it to the built-in factories therefore killed the editor worker:
// DiffEditor rendered its two panes and never produced a decoration, so the
// run diff, the commit diff and the branch diff all went blank — worse than
// the CDN this replaced. It also backs word-based suggestions, link detection
// and unicode highlighting on every editor.
//
// `getWorker` is consulted UNCONDITIONALLY (esm/vs/internal/common/workers.js
// has no `undefined` check, unlike standaloneWebWorkerService), so the default
// arm must return a real worker rather than falling through.
//
// The specifiers omit `esm/vs` on purpose: monaco 0.56's exports map rewrites
// "./X" → "./esm/vs/X", so spelling that prefix out fails to resolve.
import * as monaco from "monaco-editor";
import Editor, { DiffEditor, loader, type Monaco } from "@monaco-editor/react";

import EditorWorker from "monaco-editor/editor/editor.worker.js?worker";
import JsonWorker from "monaco-editor/languages/features/json/json.worker.js?worker";
import CssWorker from "monaco-editor/languages/features/css/css.worker.js?worker";
import HtmlWorker from "monaco-editor/languages/features/html/html.worker.js?worker";
import TsWorker from "monaco-editor/languages/features/typescript/ts.worker.js?worker";

// `MonacoEnvironment` is already declared globally by monaco-editor's own
// types, so this assigns to it rather than redeclaring the shape.
self.MonacoEnvironment = {
  getWorker(_moduleId: string, label: string): Worker {
    switch (label) {
      case "json":
        return new JsonWorker();
      case "css":
      case "scss":
      case "less":
        return new CssWorker();
      case "html":
      case "handlebars":
      case "razor":
        return new HtmlWorker();
      case "typescript":
      case "javascript":
        return new TsWorker();
      default:
        // `editorWorkerService` above all — diff, suggestions, links.
        return new EditorWorker();
    }
  },
};

loader.config({ monaco });

export { Editor, DiffEditor, monaco };
export type { Monaco };
export default Editor;
