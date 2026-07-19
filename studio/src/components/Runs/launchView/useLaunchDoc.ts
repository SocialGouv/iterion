// Extracted from LaunchView.tsx to keep that file focused.
// useLaunchDoc loads the workflow document behind the launch form — the
// ?file= path when present, otherwise the in-memory editor buffer — and
// owns the var-values form state plus the field buckets derived from
// the parsed document.

import { useEffect, useMemo, useState } from "react";

import * as filesApi from "@/api/client";
import { errorMessage } from "@/lib/errorHints";
import type { IterDocument } from "@/api/types";

import { defaultStringFor } from "@/components/shared/VarFieldInput";
import { useDocumentStore } from "@/store/document";

import { type LLMNode } from "./ModelOverridesSection";
import { pickAttachments, pickVars } from "./utils";

export function useLaunchDoc(
  filePath: string,
  onError: (msg: string) => void,
) {
  const [doc, setDoc] = useState<IterDocument | null>(null);
  const currentSource = useDocumentStore((s) => s.currentSource);
  const setCurrentSource = useDocumentStore((s) => s.setCurrentSource);
  // The in-memory editor buffer, used to launch an UNSAVED workflow (no
  // ?file= path). This is the cloud path: the server pod's rootfs is
  // read-only, so /files/save 500s and there is never an on-disk path to
  // reference — the launch API takes inline `source` instead (see
  // resolveWorkflowPath: cloud mode returns the empty file_path and runs
  // off Source). Also lets a fresh local buffer launch before its first
  // save.
  const storeDocument = useDocumentStore((s) => s.document);
  // Pristine-buffer detection: the document store initializes with a
  // default scaffold document (createEmptyDocument), so `storeDocument`
  // is never null — a bare deep-link to /runs/new would otherwise
  // silently offer launching that implicit scaffold as an "Unsaved
  // workflow". A buffer only counts as a real launch candidate once the
  // user opened a file (currentFilePath set) or edited the scaffold
  // (generation moved past the last-saved mark).
  const editorFilePath = useDocumentStore((s) => s.currentFilePath);
  const editorDirty = useDocumentStore(
    (s) => s._generation !== s._savedGeneration,
  );
  const noSource = !filePath && editorFilePath === null && !editorDirty;
  const [values, setValues] = useState<Record<string, string>>({});

  useEffect(() => {
    if (!filePath) {
      // No ?file= path — launch the unsaved editor buffer via inline
      // source. The launch API (resolveWorkflowPath) runs off Source when
      // file_path is empty, so this is a first-class path, not a fallback
      // hack; it is the ONLY way to launch in cloud mode, where the pod
      // rootfs is read-only and a workflow can never be saved to disk.
      //
      // The live edit buffer is the document store's AST (currentSource is
      // only the last opened/saved file's text, stale after edits), so we
      // derive the source from it via /api/unparse before launching.
      //
      // A pristine store buffer (never opened, never edited) is NOT a
      // launchable workflow — the render below shows a picker empty
      // state instead, so don't unparse the scaffold here.
      if (noSource) return;
      if (!storeDocument) {
        onError("No workflow to launch — open or write one in the editor first.");
        return;
      }
      let cancelled = false;
      filesApi
        .unparse(storeDocument)
        .then((src) => {
          if (cancelled) return;
          setCurrentSource(src);
          setDoc(storeDocument);
          const fields = pickVars(storeDocument);
          const initial: Record<string, string> = {};
          for (const f of fields) initial[f.name] = defaultStringFor(f);
          setValues(initial);
        })
        .catch((e) => {
          if (!cancelled) onError(errorMessage(e));
        });
      return () => {
        cancelled = true;
      };
    }
    let cancelled = false;
    filesApi
      .openFile(filePath)
      .then((res) => {
        if (cancelled) return;
        setDoc(res.document);
        setCurrentSource(res.source);
        const fields = pickVars(res.document);
        const initial: Record<string, string> = {};
        for (const f of fields) initial[f.name] = defaultStringFor(f);
        setValues(initial);
      })
      .catch((e) => {
        if (!cancelled) onError(errorMessage(e));
      });
    return () => {
      cancelled = true;
    };
  }, [filePath, noSource, onError, setCurrentSource, storeDocument]);

  // The full declared field list. Progressive-disclosure bucketing
  // (primary / bot options / auto) happens in LaunchView via
  // applyLaunchHints, because the split also folds in the resolved bot's
  // launch hints — which this hook cannot see (the bot entry is matched
  // by useBotPresets, downstream of the doc loaded here).
  const fields = pickVars(doc);

  const attachmentFields = pickAttachments(doc);

  // LLM nodes (agents + judges) the operator can retarget per run. Names are
  // the exact node ids used as override selectors. Judges first so the review
  // side reads top-down in the section.
  const llmNodes = useMemo<LLMNode[]>(() => {
    const judges: LLMNode[] = (doc?.judges ?? []).map((j) => ({
      name: j.name,
      kind: "judge",
      model: j.model,
      backend: j.backend,
    }));
    const agents: LLMNode[] = (doc?.agents ?? []).map((a) => ({
      name: a.name,
      kind: "agent",
      model: a.model,
      backend: a.backend,
    }));
    return [...judges, ...agents];
  }, [doc]);

  // Surface the worktree config so the user knows whether the
  // finalization fields will have any effect. Only the first workflow
  // is inspected (matches pickVars's selection rule).
  const worktreeMode = doc?.workflows?.[0]?.worktree ?? "";
  const worktreeOn = worktreeMode === "auto";

  return {
    doc,
    noSource,
    currentSource,
    values,
    setValues,
    fields,
    attachmentFields,
    llmNodes,
    worktreeOn,
  };
}
