import { useCallback, useEffect, useRef } from "react";
import { Link } from "wouter";
import { ExternalLinkIcon } from "@radix-ui/react-icons";

import {
  DEFAULT_WHATS_NEXT_BOT_ID,
  getFirstClassBot,
} from "@/lib/whats-next/firstClassBots";
import { useWhatsNextSession } from "@/lib/whats-next/useWhatsNextSession";
import type { FormAnswer, FormSpec } from "@/lib/whats-next/questionForm";
import type {
  IssuesSummaryMessage,
  RoadmapDoc,
  WhatsNextMessage,
} from "@/lib/whats-next/messages";

import AgentChatbox from "@/components/shared/AgentChatbox";
import { Button } from "@/components/ui/Button";
import { labelForStatus } from "@/components/Runs/runStatusMeta";
import ChatTranscript from "./ChatTranscript";
import HumanChatTurn from "./HumanChatTurn";
import PreFlightPanel from "./PreFlightPanel";
import SessionLauncher from "./SessionLauncher";

// WhatsNextView is the /whats-next route. It owns one whats-next session at a
// time via the useWhatsNextSession hook: the launcher creates the run,
// the transcript reads from the run store, and human turns are
// submitted via the hook's submitHumanAnswer.

export default function WhatsNextView() {
  const bot = getFirstClassBot(DEFAULT_WHATS_NEXT_BOT_ID);
  // Hooks must be called unconditionally — pass a dummy bot if the
  // lookup miss happens (in practice it can't since DEFAULT_WHATS_NEXT_BOT_ID
  // is a const key, but the early-return branch needs valid hook order).
  const session = useWhatsNextSession(
    bot ?? {
      id: "",
      label: "",
      description: "",
      workflowPath: "",
      launcherVars: [],
      nodeMap: {},
    },
  );

  // Stashed launcher form answer awaiting auto-submission into the
  // first matching pending human turn. Lives in a ref so the
  // auto-submit effect can read + clear it without re-rendering when
  // unrelated state changes. Operators who refresh the page mid-run
  // before the bot reaches the target turn lose the stash and answer
  // the form once it appears in the chat — acceptable degradation.
  const pendingLauncherAnswer = useRef<FormAnswer | null>(null);

  // submit is shaped to match HumanChatTurn's contract:
  //   outcome = { text, approved?, formAnswer? }
  //
  // When a node declares a rich `form:` in nodeMap, the FormAnswer's
  // question ids ARE the answer keys, so we forward verbatim.
  // Otherwise we look up textField/approvedField to build the
  // answers object from the legacy text + actions UI:
  //   ask_priorities → { context: text }
  //   human_review   → { feedback: text, approved: bool }
  const onHumanSubmit = useCallback(
    (
      messageId: string,
      outcome: {
        text: string;
        approved?: boolean;
        formAnswer?: Record<string, string | string[]>;
      },
    ) => {
      const m = bot
        ? session.messages.find((x) => x.id === messageId)
        : undefined;
      const entry = m && m.kind === "human-question" && bot
        ? bot.nodeMap[m.nodeId]
        : undefined;
      if (typeof console !== "undefined") {
        console.debug("[whats-next] onHumanSubmit", {
          messageId,
          outcome,
          hasBot: !!bot,
          hasMessage: !!m,
          messageKind: m?.kind,
          messageStatus: m?.kind === "human-question" ? m.status : undefined,
          hasEntry: !!entry,
          runId: session.runId,
        });
      }
      if (!bot || !m || m.kind !== "human-question" || !entry) return;
      // When the chat turn supplies BOTH a formAnswer (from the
      // checkbox column, e.g. human_review's selected_titles) AND
      // approved/text (from the Approve / Request-revision buttons),
      // merge the two: the form covers the structured fields the
      // bot declared in its output schema, the action covers the
      // verdict + feedback. Either alone (form-only turn, or pure
      // actions turn) keeps the existing behaviour.
      const answers: Record<string, unknown> = outcome.formAnswer
        ? { ...outcome.formAnswer }
        : {};
      if (entry.textField && outcome.text !== "") {
        answers[entry.textField] = outcome.text;
      }
      if (entry.approvedField && outcome.approved !== undefined) {
        answers[entry.approvedField] = outcome.approved;
      }
      void session.submitHumanAnswer(messageId, answers);
    },
    [bot, session],
  );

  if (!bot) {
    return (
      <div className="h-full grid place-items-center text-fg-muted">
        No first-class bot registered.
      </div>
    );
  }

  const inSession = session.status !== "idle";

  // When the engine is waiting on a human turn, render that turn at
  // the bottom (in a fixed-footer wrapper) instead of the generic
  // AgentChatbox. Avoids the inline + footer double-render by passing
  // excludeMessageId to ChatTranscript.
  const pendingHumanQuestion = session.messages.find(
    (m): m is Extract<typeof m, { kind: "human-question" }> =>
      m.kind === "human-question" && m.status === "pending",
  );
  const pendingForm = pendingHumanQuestion
    ? resolveDynamicForm(pendingHumanQuestion, session.messages, bot.nodeMap)
    : undefined;

  // Auto-submit the stashed launcher form answer into the first
  // pending human-question whose node id matches launcherFormTarget.
  // The operator picked their priority before the bot ran; once
  // explore finishes and ask_priorities surfaces, we resolve it
  // silently rather than asking again.
  useEffect(() => {
    if (!pendingHumanQuestion) return;
    if (!bot.launcherFormTarget) return;
    if (pendingHumanQuestion.nodeId !== bot.launcherFormTarget) return;
    const stash = pendingLauncherAnswer.current;
    if (!stash) return;
    pendingLauncherAnswer.current = null;
    void session.submitHumanAnswer(pendingHumanQuestion.id, stash);
  }, [pendingHumanQuestion, bot.launcherFormTarget, session]);

  return (
    <div className="h-full flex flex-col overflow-hidden">
        {!inSession ? (
          <SessionLauncher
            bot={bot}
            onLaunch={({ vars, formAnswer }) => {
              if (formAnswer) pendingLauncherAnswer.current = formAnswer;
              void session.launch(vars);
            }}
            busy={session.status === "launching"}
            errorMessage={session.errorMessage}
          />
        ) : (
          <div className="flex-1 flex flex-col max-w-3xl w-full mx-auto overflow-hidden">
            <SessionHeader bot={bot} session={session} />
            {session.messages.length === 0 ? (
              <div className="flex-1 overflow-y-auto">
                <PreFlightPanel
                  runId={session.runId}
                  runStatus={session.runStatus}
                />
              </div>
            ) : (
              <ChatTranscript
                messages={session.messages}
                bot={bot}
                onHumanSubmit={onHumanSubmit}
                busyMessageId={session.busyMessageId}
                excludeMessageId={pendingHumanQuestion?.id}
              />
            )}
            {session.errorMessage && (
              <div className="border-t border-danger/40 bg-danger-soft px-4 py-2 text-[12px] text-danger-fg">
                {session.errorMessage}
              </div>
            )}
            {session.runId &&
            (session.runStatus === "failed_resumable" ||
              session.runStatus === "cancelled") ? (
              // Terminal-but-resumable wins over PendingTurnFooter:
              // the form on a dead run is misleading — submitting goes
              // through resumeRun which would force-resume the engine
              // anyway, but the operator's mental model "the form is
              // active" is wrong. Surface Resume explicitly first; the
              // form's pending question gets re-shown by the new
              // engine instance after Resume kicks the run forward.
              <ResumeFooter
                runStatus={session.runStatus}
                busy={session.status === "submitting"}
                onResume={() => void session.resume()}
              />
            ) : pendingHumanQuestion ? (
              <PendingTurnFooter
                message={pendingHumanQuestion}
                form={pendingForm}
                busy={session.busyMessageId === pendingHumanQuestion.id}
                onSubmit={(outcome) =>
                  onHumanSubmit(pendingHumanQuestion.id, outcome)
                }
              />
            ) : (
              session.runId &&
              session.runStatus !== "finished" &&
              session.runStatus !== "failed" && (
                <AgentChatbox runId={session.runId} />
              )
            )}
          </div>
        )}
    </div>
  );
}

// resolveDynamicForm overrides the static nodeMap.form for nodes
// whose options depend on upstream output:
//   - ask_which_to_process: a checkbox per freshly-created issue
//     (from the IssuesSummaryMessage emit_action just pushed).
//   - human_review: a checkbox per proposed roadmap_item (from the
//     latest RoadmapCardMessage). Lets the operator drop individual
//     items before emit_action materialises them as kanban issues,
//     instead of having to create everything then close the ones they
//     didn't want.
// Falls back to the static nodeMap.form when the upstream message
// isn't available — under normal flow it always is by the time the
// human pause hits.
function resolveDynamicForm(
  message: Extract<WhatsNextMessage, { kind: "human-question" }>,
  messages: WhatsNextMessage[],
  nodeMap: Record<string, { form?: FormSpec } | undefined>,
): FormSpec | undefined {
  const staticForm = nodeMap[message.nodeId]?.form;
  const pendingIdx = messages.findIndex((m) => m.id === message.id);
  const upstream = pendingIdx < 0 ? messages : messages.slice(0, pendingIdx);

  if (message.nodeId === "ask_which_to_process") {
    const summary = findLatestIssuesSummary(upstream);
    if (!summary || summary.createdIssues.length === 0) return staticForm;
    return {
      mode: "flat",
      questions: [
        {
          id: "selected_issue_ids",
          kind: "checkbox",
          label: "Issues to dispatch now",
          description:
            "Tick the ones to push from backlog to ready. The dispatcher picks up ready tickets matching the configured assignee_workflows mapping.",
          options: summary.createdIssues.map((iss) => ({
            value: iss.id,
            label: iss.title,
            description: [iss.horizon, iss.assignee]
              .filter(Boolean)
              .join(" · "),
          })),
          defaultValues: summary.createdIssues.map((iss) => iss.id),
        },
        {
          id: "note",
          kind: "free_text",
          label: "Note (optional)",
          description:
            "Why this selection? Helps the bot reason about edge cases.",
          placeholder:
            "Optional — e.g. 'skip long_term, only ship next_action'",
          rows: 2,
          required: false,
        },
      ],
      submitLabel: "Dispatch selected",
    };
  }

  if (message.nodeId === "human_review") {
    const roadmap = findLatestRoadmap(upstream);
    if (!roadmap) return staticForm;
    const items = flattenRoadmapItems(roadmap);
    if (items.length === 0) return staticForm;
    return {
      mode: "flat",
      questions: [
        {
          id: "selected_titles",
          kind: "checkbox",
          label: "Items to actually create as kanban issues",
          description:
            "Pre-ticked = approve all. Untick any you don't want emit_action to materialise on the board right now; rationale + acceptance criteria still survive in the audit markdown.",
          options: items.map((it) => ({
            value: it.title,
            label: it.title,
            description: [it.horizon, it.assignee]
              .filter(Boolean)
              .join(" · "),
          })),
          defaultValues: items.map((it) => it.title),
          required: false,
        },
      ],
    };
  }

  return staticForm;
}

// flattenRoadmapItems concatenates the three horizons into a single
// list while tagging each item with its horizon — the operator's
// checkbox column should treat next_action and long_term items
// uniformly; the horizon survives as option meta so the badge is
// still visible.
function flattenRoadmapItems(
  roadmap: RoadmapDoc,
): Array<{ title: string; assignee: string; horizon: string }> {
  const out: Array<{ title: string; assignee: string; horizon: string }> = [];
  for (const it of roadmap.long_term) {
    if (it && typeof it.title === "string" && it.title.length > 0) {
      out.push({ title: it.title, assignee: it.assignee ?? "", horizon: "long_term" });
    }
  }
  for (const it of roadmap.short_term) {
    if (it && typeof it.title === "string" && it.title.length > 0) {
      out.push({ title: it.title, assignee: it.assignee ?? "", horizon: "short_term" });
    }
  }
  if (
    roadmap.next_action &&
    typeof roadmap.next_action.title === "string" &&
    roadmap.next_action.title.length > 0
  ) {
    out.push({
      title: roadmap.next_action.title,
      assignee: roadmap.next_action.assignee ?? "",
      horizon: "next_action",
    });
  }
  return out;
}

function findLatestRoadmap(messages: WhatsNextMessage[]): RoadmapDoc | null {
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i];
    if (m && m.kind === "roadmap-card") return m.roadmap;
  }
  return null;
}

function findLatestIssuesSummary(
  messages: WhatsNextMessage[],
): IssuesSummaryMessage | null {
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i];
    if (m && m.kind === "issues-summary") return m;
  }
  return null;
}

// PendingTurnFooter wraps HumanChatTurn in a Claude-Code-style fixed
// footer: top border, slightly stronger surface, comfortable padding.
// The wrapped HumanChatTurn keeps all its existing rendering (form /
// free-text / actions) — only the surround changes.
function PendingTurnFooter({
  message,
  form,
  busy,
  onSubmit,
}: {
  message: Parameters<typeof HumanChatTurn>[0]["message"];
  form: Parameters<typeof HumanChatTurn>[0]["form"];
  busy: boolean;
  onSubmit: Parameters<typeof HumanChatTurn>[0]["onSubmit"];
}) {
  return (
    <div
      className="border-t border-border-default bg-surface-1"
      role="status"
      aria-live="polite"
    >
      <div className="mx-auto max-w-3xl px-4 py-3">
        <HumanChatTurn
          message={message}
          form={form}
          busy={busy}
          onSubmit={onSubmit}
        />
      </div>
    </div>
  );
}

// ResumeFooter is the bottom-of-chat call-to-action when the run is
// in failed_resumable or cancelled state. Replaces the AgentChatbox
// (which is hidden for terminal runs) so the operator's next action
// is obvious: re-enter the run from its last checkpoint.
function ResumeFooter({
  runStatus,
  busy,
  onResume,
}: {
  runStatus: "failed_resumable" | "cancelled";
  busy: boolean;
  onResume: () => void;
}) {
  const explainer =
    runStatus === "failed_resumable"
      ? "A step failed. The run kept its checkpoint — Resume retries from that point."
      : "Run was cancelled. Resume picks up from the last checkpoint.";
  return (
    <div className="border-t border-border-default bg-surface-1">
      <div className="mx-auto max-w-3xl px-4 py-3 flex items-center gap-3">
        <div className="flex-1 text-[12px] text-fg-muted">{explainer}</div>
        <Button
          variant="primary"
          size="sm"
          disabled={busy}
          onClick={onResume}
        >
          {busy ? "…" : "Resume"}
        </Button>
      </div>
    </div>
  );
}

function SessionHeader({
  bot,
  session,
}: {
  bot: { label: string };
  session: ReturnType<typeof useWhatsNextSession>;
}) {
  return (
    <div className="px-4 py-3 border-b border-border-subtle flex items-baseline justify-between gap-3">
      <h2 className="text-[13px] font-semibold text-fg-default">
        {bot.label}
        {session.runId && (
          <span className="ml-2 text-[10px] text-fg-subtle font-mono font-normal">
            {session.runId}
          </span>
        )}
      </h2>
      <div className="flex items-baseline gap-3">
        {session.runId && (
          <Link
            href={`/runs/${encodeURIComponent(session.runId)}`}
            className="inline-flex items-center gap-1 text-[11px] text-accent hover:underline"
          >
            <ExternalLinkIcon className="w-3 h-3" />
            console
          </Link>
        )}
        <div className="text-[10px] uppercase tracking-wide text-fg-subtle">
          {humanStatus(session.status, session.runStatus)}
        </div>
        {session.status === "ended" && (
          <button
            type="button"
            onClick={session.newSession}
            className="text-[11px] text-accent hover:underline cursor-pointer"
            title="Start fresh — the current run stays in the run list."
          >
            New session
          </button>
        )}
      </div>
    </div>
  );
}

export function humanStatus(
  hi: ReturnType<typeof useWhatsNextSession>["status"],
  raw: ReturnType<typeof useWhatsNextSession>["runStatus"],
): string {
  if (hi === "launching") return "Launching…";
  if (hi === "submitting") return "Submitting…";
  if (hi === "ended") {
    const label = raw ? labelForStatus(raw) : "unknown";
    return `Ended · ${label}`;
  }
  if (raw === "paused_waiting_human") return "Waiting for your reply";
  return "Active";
}
