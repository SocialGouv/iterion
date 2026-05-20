import { useState } from "react";

import type { SurveyCardMessage } from "@/lib/whats-next/messages";

interface Props {
  message: SurveyCardMessage;
}

// SurveyCard renders the structured output of an exploration node
// (today: whats-next's `explore`). The default view is concise —
// summary + open questions — because the user is about to be asked
// for priorities and that's the information that supports the
// answer. A "Show full survey" toggle reveals the longer
// observations and the optional toplevel-dirs / recent-commits maps.

export default function SurveyCard({ message }: Props) {
  const [expanded, setExpanded] = useState(false);
  const hasExtras =
    message.observations.length > 0 ||
    message.toplevelDirs !== undefined ||
    message.recentCommits !== undefined;

  return (
    <div className="rounded-lg border border-border-default bg-surface-2 p-3 space-y-3">
      <div className="flex items-baseline justify-between gap-2">
        <h3 className="text-[13px] font-semibold text-fg-default">
          Survey report
        </h3>
        <span className="text-[10px] text-fg-subtle font-mono">
          {message.nodeId}
        </span>
      </div>

      {message.summary && (
        <div className="space-y-1">
          <div className="text-[10px] uppercase tracking-wide font-medium text-fg-muted">
            Summary
          </div>
          <p className="text-[12px] text-fg-default whitespace-pre-wrap break-words border-l-2 border-accent/40 pl-2">
            {message.summary}
          </p>
        </div>
      )}

      {message.openQuestions.length > 0 && (
        <div className="space-y-1">
          <div className="text-[10px] uppercase tracking-wide font-medium text-warning-fg">
            Open questions for you ({message.openQuestions.length})
          </div>
          <ul className="space-y-1 text-[12px] text-fg-default list-disc ml-5">
            {message.openQuestions.map((q, i) => (
              <li key={i} className="whitespace-pre-wrap break-words">
                {q}
              </li>
            ))}
          </ul>
        </div>
      )}

      {hasExtras && (
        <div className="pt-2 border-t border-border-subtle">
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="text-[11px] text-accent hover:underline cursor-pointer"
          >
            {expanded ? "Hide" : "Show"} full survey
          </button>
          {expanded && (
            <div className="mt-2 space-y-3">
              {message.observations && (
                <Section title="Observations">
                  <p className="text-[12px] text-fg-default whitespace-pre-wrap break-words">
                    {message.observations}
                  </p>
                </Section>
              )}
              {message.toplevelDirs !== undefined && (
                <Section title="Top-level directories">
                  <ValueBlock value={message.toplevelDirs} />
                </Section>
              )}
              {message.recentCommits !== undefined && (
                <Section title="Recent commits">
                  <ValueBlock value={message.recentCommits} />
                </Section>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function Section({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  return (
    <div className="space-y-1">
      <div className="text-[10px] uppercase tracking-wide font-medium text-fg-muted">
        {title}
      </div>
      <div className="rounded bg-surface-1 border border-border-subtle p-2">
        {children}
      </div>
    </div>
  );
}

function ValueBlock({ value }: { value: unknown }) {
  // Strings render verbatim; arrays render as bullets; everything
  // else falls back to JSON. Defensive — the runtime stamps whatever
  // the agent produced for these `json` fields.
  if (typeof value === "string") {
    return (
      <p className="text-[12px] text-fg-default whitespace-pre-wrap break-words">
        {value}
      </p>
    );
  }
  if (Array.isArray(value)) {
    return (
      <ul className="space-y-0.5 text-[11px] text-fg-default list-disc ml-4">
        {value.map((v, i) => (
          <li key={i} className="whitespace-pre-wrap break-words">
            {typeof v === "string" ? v : JSON.stringify(v)}
          </li>
        ))}
      </ul>
    );
  }
  return (
    <pre className="text-[11px] text-fg-default whitespace-pre-wrap break-words font-mono">
      {JSON.stringify(value, null, 2)}
    </pre>
  );
}
