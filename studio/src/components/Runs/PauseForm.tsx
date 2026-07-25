import { errorMessage } from "@/lib/errorHints";
import { useEffect, useMemo, useState } from "react";

import { resumeRun } from "@/api/runs";
import { Button, Textarea } from "@/components/ui";
import {
  askUserAllowsFreeText,
  askUserOptions,
  isReservedQuestionKey,
} from "@/lib/askUserOptions";
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
  // Overrides the workflow source sent with the resume. Run-console
  // callers omit it and the editor buffer (currentSource) is used. The
  // board caller passes `null` to send NO source — the operator isn't
  // editing this run's workflow, so the server must fall back to the
  // run's persisted FilePath instead of resuming against an unrelated
  // editor buffer.
  sourceOverride?: string | null;
  onSubmitted?: () => void;
}

// A permission-gate `ask` pause carries a structured marker under the
// reserved `_permission` key (mirrors pkg/backend/permission.Marker). When
// present we render a one-click approval card instead of free-text fields.
const PERMISSION_MARKER_KEY = "_permission";
const ASK_USER_KEY = "ask_user_response";

interface PermissionMarker {
  tool?: string;
  input?: Record<string, unknown>;
  rule?: string;
}

function permissionMarker(questions: Record<string, unknown>): PermissionMarker | null {
  const m = questions?.[PERMISSION_MARKER_KEY];
  if (m && typeof m === "object" && !Array.isArray(m)) return m as PermissionMarker;
  return null;
}

// The most identifying argument of a tool call, for compact display.
function briefInput(input?: Record<string, unknown>): string {
  if (!input) return "";
  for (const k of ["command", "file_path", "path", "url", "pattern", "query"]) {
    const v = input[k];
    if (typeof v === "string" && v) return v;
  }
  try {
    return JSON.stringify(input);
  } catch {
    return "";
  }
}

export default function PauseForm({
  runId,
  questions,
  message,
  sourceOverride,
  onSubmitted,
}: Props) {
  const marker = useMemo(() => permissionMarker(questions ?? {}), [questions]);
  const options = useMemo(() => askUserOptions(questions ?? {}), [questions]);
  // Reserved (underscore) keys are runtime plumbing (options payload,
  // permission marker, queued-messages stash) — never render them as
  // answerable fields.
  const fieldNames = useMemo(
    () => Object.keys(questions ?? {}).filter((k) => !isReservedQuestionKey(k)),
    [questions],
  );
  const [values, setValues] = useState<Record<string, string>>(() =>
    Object.fromEntries(fieldNames.map((k) => [k, ""])),
  );
  // Reset draft answers when the question set changes (e.g. a second
  // pause on the same run with different fields, or a navigation
  // between two paused runs without unmount). The lazy initialiser
  // above runs once; without this, new field names show old values
  // and old field names leak into the submit payload.
  const fieldKey = fieldNames.join("\x00");
  useEffect(() => {
    setValues(Object.fromEntries(fieldNames.map((k) => [k, ""])));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runId, fieldKey]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Answers of the last attempt the server rejected because the
  // workflow source changed since the run started; non-null renders a
  // one-click retry that replays them with force: true.
  const [forceRetry, setForceRetry] = useState<Record<string, string> | null>(
    null,
  );
  const currentSource = useDocumentStore((s) => s.currentSource);
  // An explicit prop (including null) wins over the editor buffer; null →
  // undefined so the resume carries no source and the server falls back to
  // the run's persisted FilePath.
  const resolvedSource =
    (sourceOverride !== undefined ? sourceOverride : currentSource) ?? undefined;

  const onChange = (name: string, next: string) => {
    setValues((prev) => ({ ...prev, [name]: next }));
  };

  const onSubmit = async (answers?: Record<string, string>, force = false) => {
    const payload = answers ?? values;
    setBusy(true);
    setError(null);
    setForceRetry(null);
    try {
      // The runtime accepts a generic answers map; values are passed
      // through to the resumed node's inputs. Strings are the safest
      // common type for an ad-hoc pause UI.
      await resumeRun(runId, {
        answers: payload,
        source: resolvedSource,
        ...(force ? { force: true } : {}),
      });
      onSubmitted?.();
    } catch (e) {
      const msg = errorMessage(e);
      setError(msg);
      if (/source has changed/i.test(msg)) setForceRetry(payload);
    } finally {
      setBusy(false);
    }
  };

  // Single-string answer under the ask_user key. Used by the permission
  // approval buttons ("allow"/"allow always"/"deny" become a grant rule
  // or refusal) and by the structured-options buttons (the picked
  // option's id, or typed free text).
  const decide = async (decision: string, force = false) => {
    setBusy(true);
    setError(null);
    setForceRetry(null);
    try {
      await resumeRun(runId, {
        answers: { [ASK_USER_KEY]: decision },
        source: resolvedSource,
        ...(force ? { force: true } : {}),
      });
      onSubmitted?.();
    } catch (e) {
      const msg = errorMessage(e);
      setError(msg);
      if (/source has changed/i.test(msg)) {
        setForceRetry({ [ASK_USER_KEY]: decision });
      }
    } finally {
      setBusy(false);
    }
  };

  // Shared "replay with force" affordance rendered next to every error
  // spot. The decide() path snapshots its single answer under
  // ASK_USER_KEY, so replaying through decide() keeps its semantics.
  const forceRetryButton = forceRetry && (
    <div className="flex items-center gap-2">
      <Button
        variant="primary"
        size="sm"
        disabled={busy}
        onClick={() => {
          const single = forceRetry[ASK_USER_KEY];
          if (Object.keys(forceRetry).length === 1 && single !== undefined) {
            void decide(single, true);
          } else {
            void onSubmit(forceRetry, true);
          }
        }}
      >
        Resume with updated workflow (force)
      </Button>
      <span className="text-micro text-fg-subtle">
        Replays your answer against the current workflow source.
      </span>
    </div>
  );

  if (marker) {
    const prompt = String(questions[ASK_USER_KEY] ?? "");
    const arg = briefInput(marker.input);
    return (
      <div className="space-y-3">
        {message && <p className="text-fg-muted text-micro">{message}</p>}
        <div className="rounded-md border border-warning/40 bg-warning-soft p-3 space-y-2">
          <div className="text-micro font-medium text-fg-default">
            🔐 Approval required: <code>{marker.tool}</code>
          </div>
          {arg && (
            <pre className="text-caption text-fg-subtle whitespace-pre-wrap break-all max-h-32 overflow-auto m-0">
              {arg}
            </pre>
          )}
          {!arg && prompt && (
            <div className="text-caption text-fg-subtle whitespace-pre-wrap">{prompt}</div>
          )}
        </div>
        {error && (
          <p className="text-danger-fg text-micro" role="alert">
            {error}
          </p>
        )}
        {forceRetryButton}
        <div className="flex flex-wrap gap-2">
          <Button variant="primary" size="sm" loading={busy} onClick={() => void decide("allow")}>
            Allow once
          </Button>
          <Button variant="secondary" size="sm" disabled={busy} onClick={() => void decide("allow always")}>
            Allow always
          </Button>
          <Button variant="danger" size="sm" disabled={busy} onClick={() => void decide("deny")}>
            Deny
          </Button>
        </div>
      </div>
    );
  }

  // Structured ask_user options: clickable choices (click = submit), plus
  // an optional free-text path when the tool call allowed it.
  if (options.length > 0) {
    const prompt = String(questions[ASK_USER_KEY] ?? "");
    const allowFree = askUserAllowsFreeText(questions);
    const freeDraft = values[ASK_USER_KEY] ?? "";
    return (
      <div className="space-y-3">
        {message && <p className="text-fg-muted text-micro">{message}</p>}
        {prompt && (
          <div className="text-caption text-fg-default whitespace-pre-wrap">{prompt}</div>
        )}
        <div className="flex flex-wrap gap-2">
          {options.map((o) => (
            <Button
              key={o.id}
              variant="secondary"
              size="sm"
              disabled={busy}
              onClick={() => void decide(o.id)}
            >
              {o.label}
            </Button>
          ))}
        </div>
        {allowFree && (
          <form
            onSubmit={(e) => {
              e.preventDefault();
              if (freeDraft.trim()) void decide(freeDraft);
            }}
            className="space-y-2"
          >
            <Textarea
              value={freeDraft}
              onChange={(e) => onChange(ASK_USER_KEY, e.target.value)}
              rows={2}
              spellCheck={false}
              className="text-micro"
              placeholder="Or type a custom answer…"
            />
            <Button type="submit" variant="primary" size="sm" loading={busy} disabled={!freeDraft.trim()}>
              Send
            </Button>
          </form>
        )}
        <div role="status" aria-live="polite">
          {error && (
            <p className="text-danger-fg text-micro" role="alert">
              {error}
            </p>
          )}
        </div>
        {forceRetryButton}
      </div>
    );
  }

  if (fieldNames.length === 0) {
    return (
      <div className="space-y-3">
        {message && (
          <p className="text-fg-muted text-micro">{message}</p>
        )}
        <p className="text-fg-subtle text-micro">
          This run paused without specific questions. Resume to continue.
        </p>
        <Button
          variant="primary"
          size="sm"
          onClick={() => void onSubmit()}
          loading={busy}
        >
          Resume
        </Button>
        <div role="status" aria-live="polite">
          {error && (
            <p className="text-danger-fg text-micro" role="alert">
              {error}
            </p>
          )}
        </div>
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
      {message && <p className="text-fg-muted text-micro">{message}</p>}
      {fieldNames.map((name) => {
        const prompt = String(questions[name] ?? "");
        return (
          <label key={name} className="block space-y-1">
            <div className="text-micro font-medium text-fg-default">{name}</div>
            {prompt && (
              <div className="text-caption text-fg-subtle whitespace-pre-wrap">
                {prompt}
              </div>
            )}
            <Textarea
              value={values[name] ?? ""}
              onChange={(e) => onChange(name, e.target.value)}
              rows={prompt.length > 80 ? 4 : 2}
              spellCheck={false}
              className="text-micro"
            />
          </label>
        );
      })}
      {error && (
        <p className="text-danger-fg text-micro" role="alert">
          {error}
        </p>
      )}
      {forceRetryButton}
      <div className="flex gap-2">
        <Button type="submit" variant="primary" size="sm" loading={busy}>
          Submit &amp; Resume
        </Button>
      </div>
    </form>
  );
}
