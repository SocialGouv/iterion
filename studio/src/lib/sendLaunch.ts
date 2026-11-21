// The one place a launch is sent from, whatever surface asks for it — the
// launch form, a bot's Test pane. It holds the target while the launch is in
// flight, under a key every surface computes the same way for the same thing,
// and it never lets an answer go unheard: when the surface that asked is no
// longer on screen at the answer, the outcome is announced in a notice — the
// run that started and the way to it, the refusal, or that nothing answered.
// What the surface still on screen does with its own answer stays its own.

import { ApiError, currentRequestTeamGeneration, currentSessionGeneration, SessionRenewalUnavailableError } from "@/api/client";
import { errorMessage } from "@/lib/errorHints";
import { absolutePath } from "@/lib/launchSource";
import { useLaunchesInFlight } from "@/store/launchesInFlight";
import { useUIStore } from "@/store/ui";

// A target's key is what the SERVER resolved, never a spelling the caller
// typed: the bot's canonical id as the registry or the launch-source read
// answered it, within the team the session is scoped to — the same slug in
// two teams is two bots. Locally a bot is its workflow file, the RESOLVED
// one: `bots/x/main.bot`, `./bots/x/main.bot` and its absolute path under the
// working directory are one file. The editor's unsaved buffer is its tab's.
export function botLaunchKey(teamID: string, botID: string): string {
  return `bot\u0000${teamID}\u0000${botID}`;
}
export function fileLaunchKey(teamID: string, path: string, workDir?: string | null): string {
  return `file\u0000${teamID}\u0000${absolutePath(path, workDir ?? undefined, true) ?? path}`;
}
export function bufferLaunchKey(tabID: string): string {
  return `buffer\u0000${tabID}`;
}

export interface LaunchTarget {
  /** The in-flight key (botLaunchKey / fileLaunchKey / bufferLaunchKey). */
  key: string;
  /** What a notice calls the target: "the bot nestor", a file path. */
  label: string;
  /** Where the target's runs are listed, for an outcome nobody heard. */
  runsHref: string;
}

export type LaunchOutcome =
  | { kind: "started"; runID: string }
  /** The step before the launch failed: nothing was sent, no run exists. */
  | { kind: "not-started"; error: unknown }
  /** The launch was sent and drew an error: a refusal, or no answer at all. */
  | { kind: "failed"; error: unknown };

/** What an outcome MEANS for the run, said once for every surface and for
 *  both places it can be heard — the surface that asked, or a notice when
 *  that surface is gone:
 *    - "started": a run exists;
 *    - "refused": the server said no (an API answer in the 4xx range) — no
 *      run exists;
 *    - "not-started": nothing was sent, or the request never reached a
 *      launch — no run exists;
 *    - "unanswered": nothing said anything of the run. It may exist, and be
 *      billed: a second launch is the operator's to decide, knowing it. */
export interface LaunchNotice {
  kind: "started" | "refused" | "not-started" | "unanswered";
  /** The outcome in a sentence that names the target. */
  message: string;
  /** For "unanswered": where the target's runs are listed. */
  runsHref?: string;
}

export interface SentLaunch {
  outcome: LaunchOutcome;
  notice: LaunchNotice;
  /** Whether the surface that asked is still the one on screen. When it is
   *  not, the notice was announced and the outcome is not the surface's to
   *  apply. */
  onScreen: boolean;
}

/** Whether the server REFUSED the launch — an API answer in the 4xx range.
 *  Anything else (a network failure, a gateway's 5xx) says nothing of the
 *  run: it may exist. */
export function launchWasRefused(error: unknown): boolean {
  return error instanceof ApiError && error.status >= 400 && error.status < 500;
}

/** The sentence of a failed launch: the server's own where it wrote one for
 *  this refusal (the bot changed under the form), the error's otherwise. */
export function launchFailureSentence(error: unknown): string {
  if (error instanceof ApiError && error.errorCode === "bot_source_changed") {
    return error.detail ?? errorMessage(error);
  }
  return errorMessage(error);
}

/** The one classification of a launch's outcome (LaunchNotice). */
export function launchNotice(
  target: LaunchTarget,
  outcome: LaunchOutcome,
  preparing: string,
): LaunchNotice {
  switch (outcome.kind) {
    case "started":
      return { kind: "started", message: `Run ${outcome.runID} started for ${target.label}.` };
    case "not-started":
      return {
        kind: "not-started",
        message: `The launch of ${target.label} did not start: ${preparing} failed — ${errorMessage(outcome.error)}`,
      };
    case "failed":
      if (outcome.error instanceof SessionRenewalUnavailableError) {
        // The request was answered 401 and the session could not be renewed:
        // the server launched nothing.
        return {
          kind: "not-started",
          message: `The launch of ${target.label} did not start: ${errorMessage(outcome.error)}`,
        };
      }
      if (launchWasRefused(outcome.error)) {
        return {
          kind: "refused",
          message: `The launch of ${target.label} was refused: ${launchFailureSentence(outcome.error)}`,
        };
      }
      // Nothing said the run does not exist: inviting a second launch would
      // start a second run beside one that may be running, and billed.
      return {
        kind: "unanswered",
        message: `No answer came for the launch of ${target.label} (${errorMessage(outcome.error)}): the run may have started — check its runs before launching again.`,
        runsHref: target.runsHref,
      };
  }
}

// Every notice of a launch carries this key prefix: it is what an account
// change forgets (forgetLaunches).
const NOTICE = "launch:";
let notices = 0;

function announce(
  notice: LaunchNotice,
  outcome: LaunchOutcome,
  navigate: (path: string) => void,
) {
  const { addToast } = useUIStore.getState();
  if (outcome.kind === "started") {
    const runPath = `/runs/${encodeURIComponent(outcome.runID)}`;
    addToast(notice.message, "success", {
      persistent: true,
      key: `${NOTICE}${outcome.runID}`,
      action: { label: "Open the run", onClick: () => navigate(runPath) },
    });
    return;
  }
  const runsHref = notice.runsHref;
  addToast(notice.message, runsHref ? "warning" : "error", {
    persistent: true,
    key: `${NOTICE}${notice.kind}:${++notices}`,
    ...(runsHref
      ? { action: { label: "Open its runs", onClick: () => navigate(runsHref) } }
      : {}),
  });
}

export async function sendLaunch<P = undefined>(opts: {
  target: LaunchTarget;
  /** Asked when the answer lands: is the surface that asked still showing
   *  this target? */
  onScreen: () => boolean;
  navigate: (path: string) => void;
  /** A step the launch needs first — creating its repository. The target is
   *  held from before it, and its failure is "the launch did not start". */
  prepare?: () => Promise<P>;
  /** What `prepare` does, for the notice: "creating the repository". */
  preparing?: string;
  send: (prepared: P | undefined) => Promise<{ run_id: string }>;
}): Promise<SentLaunch> {
  const { target } = opts;
  const flights = useLaunchesInFlight.getState();
  const sitting = flights.sitting;
  const team = currentRequestTeamGeneration();
  const session = currentSessionGeneration();
  const sameScope = () =>
    useLaunchesInFlight.getState().sitting === sitting &&
    currentRequestTeamGeneration() === team &&
    currentSessionGeneration() === session;
  flights.begin(target.key);
  let outcome: LaunchOutcome;
  try {
    let prepared: P | undefined;
    let notStarted: LaunchOutcome | null = null;
    if (opts.prepare) {
      try {
        prepared = await opts.prepare();
      } catch (error) {
        notStarted = { kind: "not-started", error };
      }
    }
    if (notStarted) {
      outcome = notStarted;
    } else if (!sameScope()) {
      outcome = { kind: "not-started", error: new Error("The account or team changed before the launch was sent.") };
    } else {
      try {
        outcome = { kind: "started", runID: (await opts.send(prepared)).run_id };
      } catch (error) {
        outcome = { kind: "failed", error };
      }
    }
  } finally {
    useLaunchesInFlight.getState().end(target.key, sitting);
  }
  // The result belongs to its original account and team. Never apply it or
  // send the operator to a runs page in a different scope.
  const notice = launchNotice(target, outcome, opts.preparing ?? "preparing it");
  if (!sameScope()) return { outcome, notice, onScreen: false };
  const onScreen = opts.onScreen();
  if (!onScreen) {
    announce(notice, outcome, (path) => {
      if (sameScope()) opts.navigate(path);
    });
  }
  return { outcome, notice, onScreen };
}

/** Whether a launch of this target is in flight — from any surface. An empty
 *  key is a target not resolved yet: nothing holds it. */
export function useLaunchInFlight(key: string): boolean {
  return useLaunchesInFlight((s) => key !== "" && (s.byTarget[key] ?? 0) > 0);
}

/** The account changed: one account's launches are neither held nor named in
 *  the next one's session. */
export function forgetLaunches(): void {
  useLaunchesInFlight.getState().forget();
  clearLaunchNotices();
}

export function clearLaunchNotices(): void {
  useUIStore.setState((s) => ({ toasts: s.toasts.filter((t) => !t.key?.startsWith(NOTICE)) }));
}
