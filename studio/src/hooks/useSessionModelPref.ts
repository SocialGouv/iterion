import { errorMessage } from "@/lib/errorHints";
import { useCallback, useEffect, useRef, useState } from "react";

import {
  clearModelPref,
  fetchModelPref,
  saveModelPref,
  type ModelPref,
} from "@/api/modelPrefs";

// useSessionModelPref owns the remembered model choice for one scope key (the
// assistant passes its bot id).
//
// It is deliberately NOT a react-query hook: the value is read once per mount
// and written on an explicit operator action, and — critically — the launch
// path needs to read the CURRENT choice synchronously, without racing a
// refetch. A ref holds the authoritative value for that read; state drives the
// render.
//
// A server with no prefs store 404s, and a browser can be offline. Neither can
// be allowed to block a session, so failures degrade to "no preference" and are
// reported without being thrown.

export interface SessionModelChoice {
  model?: string;
  backend?: string;
  effort?: string;
}

export interface UseSessionModelPrefResult {
  choice: SessionModelChoice;
  // set is true when a choice has been recorded, which is different from a
  // recorded-but-empty one ("use the bot's default, and I meant it").
  set: boolean;
  loading: boolean;
  saving: boolean;
  error: string | null;
  // available is false when this server cannot persist a preference. The UI
  // still lets the operator choose — it just says the choice is per-session.
  available: boolean;
  save: (choice: SessionModelChoice) => Promise<void>;
  reset: () => Promise<void>;
  // current reads the authoritative choice synchronously, for the launch path.
  current: () => SessionModelChoice;
}

const EMPTY: SessionModelChoice = {};

interface PreferenceLoadState {
  key: string | null;
  choice: SessionModelChoice;
  set: boolean;
  loading: boolean;
  error: string | null;
  available: boolean;
}

function toChoice(p: ModelPref | null): SessionModelChoice {
  if (!p) return EMPTY;
  return { model: p.model, backend: p.backend, effort: p.effort };
}

export function useSessionModelPref(key: string | null): UseSessionModelPrefResult {
  const [loaded, setLoaded] = useState<PreferenceLoadState>({
    key,
    choice: EMPTY,
    set: false,
    loading: !!key,
    error: null,
    available: true,
  });
  const [saving, setSaving] = useState(false);
  const choiceRef = useRef<{ key: string | null; choice: SessionModelChoice }>({
    key,
    choice: EMPTY,
  });

  // Key the rendered state too: a rerender for bot B must not show bot A's
  // choice during the gap before B's passive fetch effect starts.
  const active: PreferenceLoadState =
    loaded.key === key
      ? loaded
      : {
          key,
          choice: EMPTY,
          set: false,
          loading: !!key,
          error: null,
          available: true,
        };

  useEffect(() => {
    // A preference belongs to exactly one key. Clear the synchronous launch
    // value before loading the next key so a bot switch can never launch with
    // the previous bot's model while this request is in flight (or forever
    // when the new key is unavailable).
    choiceRef.current = { key, choice: EMPTY };
    if (!key) return;
    const ac = new AbortController();
    let live = true;
    fetchModelPref(key, { signal: ac.signal })
      .then((p) => {
        if (!live) return;
        const c = toChoice(p);
        choiceRef.current = { key, choice: c };
        setLoaded({
          key,
          choice: c,
          set: p.set,
          loading: false,
          error: null,
          available: true,
        });
      })
      .catch((e) => {
        if (!live || ac.signal.aborted) return;
        // A server without the store is a normal configuration, not an error
        // worth shouting about — the session simply cannot remember.
        setLoaded({
          key,
          choice: EMPTY,
          set: false,
          loading: false,
          error: errorMessage(e),
          available: false,
        });
      });
    return () => {
      live = false;
      ac.abort();
    };
  }, [key]);

  const save = useCallback(
    async (next: SessionModelChoice) => {
      // Apply locally first: a server that cannot persist must still let the
      // operator retarget the session they are about to launch.
      choiceRef.current = { key, choice: next };
      setLoaded((prev) => ({
        key,
        choice: next,
        set: true,
        loading: false,
        error: null,
        available: prev.key === key ? prev.available : true,
      }));
      if (!key) return;
      setSaving(true);
      try {
        await saveModelPref({ key, ...next });
        setLoaded((prev) =>
          prev.key === key ? { ...prev, available: true } : prev,
        );
      } catch (e) {
        setLoaded((prev) =>
          prev.key === key
            ? { ...prev, available: false, error: errorMessage(e) }
            : prev,
        );
      } finally {
        setSaving(false);
      }
    },
    [key],
  );

  const reset = useCallback(async () => {
    choiceRef.current = { key, choice: EMPTY };
    setLoaded({
      key,
      choice: EMPTY,
      set: false,
      loading: false,
      error: null,
      available: true,
    });
    if (!key) return;
    setSaving(true);
    try {
      await clearModelPref(key);
    } catch (e) {
      setLoaded((prev) =>
        prev.key === key ? { ...prev, error: errorMessage(e) } : prev,
      );
    } finally {
      setSaving(false);
    }
  }, [key]);

  const current = useCallback(
    () => (choiceRef.current.key === key ? choiceRef.current.choice : EMPTY),
    [key],
  );

  return {
    choice: active.choice,
    set: active.set,
    loading: active.loading,
    saving,
    error: active.error,
    available: active.available,
    save,
    reset,
    current,
  };
}
