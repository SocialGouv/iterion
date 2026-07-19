// Extracted from BotBuilder/index.tsx to keep that file focused.
// useBuilderDraft owns the builder's cross-phase state: the draft
// (seeded from localStorage, auto-saved on every change), the
// "Draft saved" hint, and the created-bot handoff that clears the
// draft and refreshes the shared bots store.

import { useEffect, useRef, useState } from "react";

import type { BotEntryWithSchema } from "@/api/bots";
import { useBotsStore } from "@/store/bots";

import { DRAFT_KEY, loadDraft, type BuilderDraft } from "./model";

export function useBuilderDraft() {
  const [draft, setDraft] = useState<BuilderDraft>(loadDraft);
  const [created, setCreated] = useState<BotEntryWithSchema | null>(null);
  const [draftSaved, setDraftSaved] = useState(false);

  // Auto-save the draft on every change (skip the initial mount and
  // anything after a successful create — the draft is cleared then).
  const firstRender = useRef(true);
  useEffect(() => {
    if (created) return;
    if (firstRender.current) {
      firstRender.current = false;
      return;
    }
    try {
      localStorage.setItem(DRAFT_KEY, JSON.stringify(draft));
    } catch {
      return; // quota/private-mode — the form still works, just unsaved
    }
    // Hint shown/hidden from timers (not synchronously in the effect
    // body) so the write→feedback flow doesn't cascade a re-render.
    const show = window.setTimeout(() => setDraftSaved(true), 0);
    const hide = window.setTimeout(() => setDraftSaved(false), 2000);
    return () => {
      window.clearTimeout(show);
      window.clearTimeout(hide);
    };
  }, [draft, created]);

  const onCreated = (entry: BotEntryWithSchema) => {
    try {
      localStorage.removeItem(DRAFT_KEY);
    } catch {
      /* non-fatal */
    }
    setCreated(entry);
    // Refresh the shared bots store so /bots and /bots/<slug> see the
    // new entry immediately.
    void useBotsStore.getState().refetch();
  };

  return { draft, setDraft, draftSaved, created, onCreated };
}
