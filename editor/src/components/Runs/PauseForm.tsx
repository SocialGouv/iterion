import { useMemo, useState } from "react";

import { resumeRun } from "@/api/runs";
import { Button, Textarea } from "@/components/ui";
import { useDocumentStore } from "@/store/document";

interface Props {
  runId: string;
  // Map of field name → question text. Mirror of
  // store.Checkpoint.InteractionQuestions / human_input_requested
  // event payload.
  questions: Record<string, unknown>;
  // Optional one-line description that the agent surfaced on pause
  // (e.g. "Awaiting your approval to merge"). Comes from event data.
  message?: string;
  onSubmitted?: () => void;
}

export default function PauseForm({ runId, questions, message, onSubmitted }: Props) {
  const fieldNames = useMemo(() => Object.keys(questions ?? {}), [questions]);
  const [values, setValues] = useState<Record<string, string>>(() =>
    Object.fromEntries(fieldNames.map((k) => [k, ""])),
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const currentSource = useDocumentStore((s) => s.currentSource);

  const onChange = (name: string, next: string) => {
    setValues((prev) => ({ ...prev, [name]: next }));
  };

  const onSubmit = async () => {
    setBusy(true);
    setError(null);
    try {
      // The runtime accepts a generic answers map; values are passed
      // through to the resumed node's inputs. Strings are the safest
      // common type for an ad-hoc pause UI.
      await resumeRun(runId, { answers: values, source: currentSource ?? undefined });
      onSubmitted?.();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  if (fieldNames.length === 0) {
    return (
      <div className="space-y-3">
        {message && (
          <p className="text-fg-muted text-[11px]">{message}</p>
        )}
        <p className="text-fg-subtle text-[11px]">
          This run paused without specific questions. Resume to continue.
        </p>
        <Button
          variant="primary"
          size="sm"
          onClick={() => void onSubmit()}
          disabled={busy}
        >
          {busy ? "Resuming…" : "Resume"}
        </Button>
        {error && (
          <p className="text-danger-fg text-[11px]" role="alert">
            {error}
          </p>
        )}
      </div>
    );
  }

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        void onSubmit();
      }}
      className="space-y-3"
    >
      {message && <p className="text-fg-muted text-[11px]">{message}</p>}
      {fieldNames.map((name) => {
        const prompt = String(questions[name] ?? "");
        return (
          <label key={name} className="block space-y-1">
            <div className="text-[11px] font-medium text-fg-default">{name}</div>
            {prompt && (
              <div className="text-[10px] text-fg-subtle whitespace-pre-wrap">
                {prompt}
              </div>
            )}
            <Textarea
              value={values[name] ?? ""}
              onChange={(e) => onChange(name, e.target.value)}
              rows={prompt.length > 80 ? 4 : 2}
              spellCheck={false}
              className="text-[11px]"
            />
          </label>
        );
      })}
      {error && (
        <p className="text-danger-fg text-[11px]" role="alert">
          {error}
        </p>
      )}
      <div className="flex gap-2">
        <Button type="submit" variant="primary" size="sm" disabled={busy}>
          {busy ? "Resuming…" : "Submit & Resume"}
        </Button>
      </div>
    </form>
  );
}
