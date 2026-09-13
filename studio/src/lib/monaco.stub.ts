// Test double for `@/lib/monaco`, wired by the `test.alias` in vite.config.ts.
//
// The real module imports `monaco-editor`, whose module graph touches `window`
// and `document.queryCommandSupported` at IMPORT time. Vitest runs this suite
// under Node by default, so any test that transitively reaches an editor
// component — a card drawer, a commits panel — would die on `ReferenceError:
// window is not defined` while testing something else entirely.
//
// This re-exports the same surface from `@monaco-editor/react` alone, which is
// what those tests loaded before Monaco was bundled: a real component that
// renders its loading state and never instantiates an editor. Stubbing the
// EDITOR would risk a component test passing vacuously; stubbing only the
// bundled monaco INSTANCE removes nothing the tests assert.
//
// Monaco's own behaviour is covered where it can be: in a browser, by
// studio/e2e/specs/security-headers.spec.ts.
import Editor, { DiffEditor, loader, type Monaco } from "@monaco-editor/react";

export { Editor, DiffEditor, loader };
export type { Monaco };
export default Editor;

// The real module exports the bundled namespace; nothing under test reads it.
export const monaco = undefined as unknown as typeof import("monaco-editor");
