// The studio's editor entry point. Import `Editor` / `DiffEditor` from HERE,
// never from `@monaco-editor/react` (which would re-arm the CDN fetch) and
// never from `./monacoInstance` (which would put 4.2 MB on the importer's
// critical path).
//
// `./monacoInstance` holds the real thing: the bundled `monaco-editor`, its
// five workers, and the `loader.config({ monaco })` that keeps anything from
// being fetched at runtime. Everything there is EAGER by construction — one
// static import of it anywhere pulls the whole editor into that module's
// chunk.
//
// So this module re-exports the same names as React.lazy components with their
// own Suspense boundary. Semantics the call sites already rely on are
// unchanged: the dialogs stay mounted (so Radix keeps its exit animation) and
// simply render nothing while closed, and monaco is fetched at the moment an
// editor first RENDERS — which, for a dialog, is the moment it opens.
//
// Before this split, `/runs` and `/login` downloaded and parsed 4.2 MB of
// editor plus a 158 KB render-blocking stylesheet for an editor they never
// mount: the run view statically imports the diff and edit dialogs, and the
// toolbar the bundle drawer.
import { lazy, Suspense, type ComponentProps } from "react";
import type { Monaco } from "@monaco-editor/react";

const LazyEditor = lazy(() =>
  import("./monacoInstance").then((m) => ({ default: m.Editor })),
);
const LazyDiffEditor = lazy(() =>
  import("./monacoInstance").then((m) => ({ default: m.DiffEditor })),
);

// The editor paints its own loading state once mounted; this covers only the
// chunk fetch, and matches the surrounding surface rather than flashing white.
function EditorFallback() {
  return <div className="h-full w-full bg-surface-0" aria-busy="true" />;
}

export function Editor(props: ComponentProps<typeof LazyEditor>) {
  return (
    <Suspense fallback={<EditorFallback />}>
      <LazyEditor {...props} />
    </Suspense>
  );
}

export function DiffEditor(props: ComponentProps<typeof LazyDiffEditor>) {
  return (
    <Suspense fallback={<EditorFallback />}>
      <LazyDiffEditor {...props} />
    </Suspense>
  );
}

export type { Monaco };
export default Editor;
