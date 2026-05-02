import { useEffect, useMemo, useState } from "react";
import { useLocation, useSearch } from "wouter";

import * as filesApi from "@/api/client";
import { createRun } from "@/api/runs";
import type { IterDocument, VarField } from "@/api/types";
import { Button } from "@/components/ui/Button";

import VarFieldInput, { defaultStringFor } from "./VarFieldInput";
import { isPromptLikeVar } from "@/lib/promptVarHeuristics";

/** Read the workflow's vars (workflow-level if a single workflow is
 *  declared, else the file-level `vars:` block). */
function pickVars(doc: IterDocument | null): VarField[] {
  if (!doc) return [];
  const wf = doc.workflows?.[0];
  if (wf?.vars?.fields?.length) return wf.vars.fields;
  return doc.vars?.fields ?? [];
}

export default function LaunchView() {
  const [, setLocation] = useLocation();
  const search = useSearch();
  const filePath = useMemo(() => {
    const params = new URLSearchParams(search);
    return params.get("file") ?? "";
  }, [search]);

  const [doc, setDoc] = useState<IterDocument | null>(null);
  const [values, setValues] = useState<Record<string, string>>({});
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    if (!filePath) {
      setError("Missing ?file=<path> query parameter");
      return;
    }
    let cancelled = false;
    filesApi
      .openFile(filePath)
      .then((res) => {
        if (cancelled) return;
        setDoc(res.document);
        const fields = pickVars(res.document);
        const initial: Record<string, string> = {};
        for (const f of fields) initial[f.name] = defaultStringFor(f);
        setValues(initial);
      })
      .catch((e) => {
        if (!cancelled) setError((e as Error).message);
      });
    return () => {
      cancelled = true;
    };
  }, [filePath]);

  const fields = pickVars(doc);

  const onSubmit = async () => {
    setSubmitting(true);
    setError(null);
    try {
      const res = await createRun({ file_path: filePath, vars: values });
      setLocation(`/runs/${encodeURIComponent(res.run_id)}`);
    } catch (e) {
      setError((e as Error).message);
      setSubmitting(false);
    }
  };

  return (
    <div className="h-full flex flex-col bg-surface-1 text-fg-default">
      <header className="border-b border-border-default px-4 py-3 flex items-center gap-3">
        <h1 className="text-sm font-bold">Launch run</h1>
        <span className="text-xs text-fg-subtle font-mono truncate">{filePath}</span>
        <button
          className="ml-auto text-xs px-2 py-1 rounded bg-surface-2 hover:bg-surface-3"
          onClick={() => setLocation("/edit")}
        >
          Cancel
        </button>
      </header>

      <div className="flex-1 overflow-auto px-4 py-4 max-w-3xl">
        {error && (
          <div className="mb-3 px-3 py-2 rounded bg-danger-soft text-danger-fg text-xs">{error}</div>
        )}
        {!doc && !error ? (
          <div className="text-xs text-fg-subtle">Loading workflow…</div>
        ) : (
          <>
            {fields.length === 0 ? (
              <p className="text-xs text-fg-subtle">
                This workflow declares no input vars. You can launch it as-is.
              </p>
            ) : (
              <form
                onSubmit={(e) => {
                  e.preventDefault();
                  if (!submitting) void onSubmit();
                }}
              >
                <h2 className="text-xs font-medium text-fg-muted mb-2">Inputs</h2>
                <div className="space-y-4">
                  {fields.map((f) => {
                    const promptLike = isPromptLikeVar(f);
                    const noDefault = !f.default;
                    if (promptLike) {
                      return (
                        <div key={f.name} className="flex flex-col gap-1.5">
                          <label htmlFor={`var-${f.name}`} className="flex items-baseline gap-2">
                            <span className="text-xs font-medium font-mono text-fg-default">{f.name}</span>
                            <span className="text-[10px] text-fg-subtle">{f.type}</span>
                            {noDefault && (
                              <span className="text-[10px] text-warning-fg uppercase tracking-wide">required</span>
                            )}
                          </label>
                          <VarFieldInput
                            field={f}
                            value={values[f.name] ?? ""}
                            onChange={(v) =>
                              setValues((prev) => ({ ...prev, [f.name]: v }))
                            }
                          />
                        </div>
                      );
                    }
                    return (
                      <div key={f.name} className="grid grid-cols-[160px_1fr] gap-3 items-start">
                        <label htmlFor={`var-${f.name}`} className="pt-1">
                          <div className="text-xs font-medium font-mono">{f.name}</div>
                          <div className="text-[10px] text-fg-subtle">{f.type}</div>
                        </label>
                        <VarFieldInput
                          field={f}
                          value={values[f.name] ?? ""}
                          onChange={(v) =>
                            setValues((prev) => ({ ...prev, [f.name]: v }))
                          }
                        />
                      </div>
                    );
                  })}
                </div>
              </form>
            )}
            <div className="mt-6 flex items-center gap-2">
              <Button
                variant="primary"
                onClick={() => void onSubmit()}
                disabled={submitting || !doc}
              >
                {submitting ? "Launching…" : "Launch"}
              </Button>
              <span className="text-[10px] text-fg-subtle">
                Run ID is generated automatically.
              </span>
            </div>
          </>
        )}
      </div>
    </div>
  );
}
