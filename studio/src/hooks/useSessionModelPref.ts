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

function toChoice(p: ModelPref | null): SessionModelChoice {
  if (!p) return EMPTY;
  return { model: p.model, backend: p.backend, effort: p.effort };
}

export function useSessionModelPref(key: string | null): UseSessionModelPrefResult {
  const [choice, setChoice] = useState<SessionModelChoice>(EMPTY);
  const [set, setSet] = useState(false);
  const [loading, setLoading] = useState(!!key);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [available, setAvailable] = useState(true);
  const choiceRef = useRef<SessionModelChoice>(EMPTY);

  useEffect(() => {
    if (!key) {
      setLoading(false);
      return;
    }
    const ac = new AbortController();
    let live = true;
    setLoading(true);
    fetchModelPref(key, { signal: ac.signal })
      .then((p) => {
        if (!live) return;
        const c = toChoice(p);
        choiceRef.current = c;
        setChoice(c);
        setSet(p.set);
        setAvailable(true);
      })
      .catch((e) => {
        if (!live || ac.signal.aborted) return;
        // A server without the store is a normal configuration, not an error
        // worth shouting about — the session simply cannot remember.
        setAvailable(false);
        setError(errorMessage(e));
      })
      .finally(() => {
        if (live) setLoading(false);
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
      choiceRef.current = next;
      setChoice(next);
      setSet(true);
      if (!key) return;
      setSaving(true);
      setError(null);
      try {
        await saveModelPref({ key, ...next });
        setAvailable(true);
      } catch (e) {
        setAvailable(false);
        setError(errorMessage(e));
      } finally {
        setSaving(false);
      }
    },
    [key],
  );

  const reset = useCallback(async () => {
    choiceRef.current = EMPTY;
    setChoice(EMPTY);
    setSet(false);
    if (!key) return;
    setSaving(true);
    setError(null);
    try {
      await clearModelPref(key);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setSaving(false);
    }
  }, [key]);

  const current = useCallback(() => choiceRef.current, []);

  return { choice, set, loading, saving, error, available, save, reset, current };
}
