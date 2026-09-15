// A suggested assistant reply may require another Studio surface.
//
// The model supplies a typed reference, never an href. The Studio resolves it,
// navigates, waits for the destination context, and only then sends the
// operator's chosen message.

import { useCallback, useEffect, useRef, useState } from "react";
import { useLocation } from "wouter";

import { ApiError, openFile } from "@/api/client";
import { captureActiveEditorDocument } from "@/lib/chatDock/editorSession";
import {
  hrefForReference,
  mintReference,
  type ReferenceKind,
  type TypedReference,
} from "@/lib/chatDock/routeReference";
import type { AssistantQuickReply } from "@/lib/whats-next/useAssistantComposer";

// Fallback reply when Copi offered the editor but no more specific suggested
// reply exists. Kept stable because the bot recognises it as consent to build.
export const EDITOR_OPENED_CONFIRMATION = "Opened the editor — go ahead.";
const NAVIGATION_REPLY_TTL_MS = 30_000;

interface PendingNavigationReply {
  message: string;
  targetRef: string;
  href: string;
  startedAt: number;
}

export interface NavigationReplyController {
  submit: (message: string, targetRef: string) => void;
  busy: boolean;
  error: string | null;
}

function botPreflightError(path: string, cause: unknown): string {
  if (cause instanceof ApiError) {
    if (cause.status === 404) {
      return `The workflow ${path} does not exist in the current workspace. Nothing was sent to the assistant.`;
    }
    if (cause.status === 400) {
      return `The workflow ${path} is not a valid file in the current workspace. Nothing was sent to the assistant.`;
    }
  }
  return `The workflow ${path} could not be verified in the current workspace. Nothing was sent to the assistant.`;
}

const REFERENCE_KINDS = new Set<ReferenceKind>([
  "run",
  "node",
  "card",
  "bot",
  "repo",
  "view",
]);

/** Resolve only a structurally valid typed reference through Studio routes. */
export function hrefForAssistantReplyTarget(targetRef: string): string | null {
  const slash = targetRef.indexOf("/");
  if (slash <= 0) return null;
  const rawKind = targetRef.slice(0, slash);
  const id = targetRef.slice(slash + 1);
  if (!REFERENCE_KINDS.has(rawKind as ReferenceKind)) return null;
  // Only a bundle workflow establishes the editor/authoring context that a
  // navigation reply promises. A source helper can open in Studio but has no
  // bundle manifest snapshot, which used to strand Copi in a fake blocker.
  if (rawKind === "bot" && !id.endsWith(".bot")) return null;
  const minted = mintReference(rawKind as ReferenceKind, id, id);
  if (!minted || minted.ref !== targetRef) return null;
  return hrefForReference(minted.ref);
}

export function editorTargetForPage(reference: TypedReference | null): string {
  return reference?.kind === "bot" ? reference.ref : "view/editor";
}

/**
 * Existing conversations may still contain string-only replies emitted beside
 * the retired venue button. Fuse those legacy replies with the venue until the
 * turn drains; typed replies decide explicitly whether they navigate.
 */
export function navigationTargetForReply(
  reply: AssistantQuickReply,
  editorVenue: boolean,
  reference: TypedReference | null,
): string | null {
  if (reply.navigateTo) return reply.navigateTo;
  if (reply.legacy && editorVenue) return editorTargetForPage(reference);
  return null;
}

/**
 * Navigate to a Studio-owned typed reference, then send with fresh context.
 *
 * A bot reference is stronger than merely reaching /editor: the hook waits
 * until that exact document is active and fully serialisable. This closes the
 * race where a suggestion reached Copi while EditorTabsView was still opening
 * or hydrating the file.
 */
export function useNavigationReply(
  send: (message: string, targetRef: string) => Promise<unknown>,
): NavigationReplyController {
  const [route, setLocation] = useLocation();
  const [pending, setPending] = useState<PendingNavigationReply | null>(null);
  const [preflighting, setPreflighting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const preflightRequestRef = useRef(0);
  const sendingRef = useRef(false);
  const sendRef = useRef(send);
  useEffect(() => {
    sendRef.current = send;
  }, [send]);

  useEffect(
    () => () => {
      preflightRequestRef.current += 1;
    },
    [],
  );

  const submit = useCallback(
    (message: string, targetRef: string) => {
      const requestID = ++preflightRequestRef.current;
      const href = hrefForAssistantReplyTarget(targetRef);
      if (!href) {
        setPreflighting(false);
        setError("This assistant destination is not available in the Studio.");
        return;
      }
      const startNavigation = () => {
        if (preflightRequestRef.current !== requestID) return;
        setPreflighting(false);
        setError(null);
        setPending({
          message,
          targetRef,
          href,
          startedAt: Date.now(),
        });
        setLocation(href);
      };
      const expectedEditorFile = targetRef.startsWith("bot/")
        ? targetRef.slice("bot/".length)
        : null;
      if (!expectedEditorFile) {
        startNavigation();
        return;
      }

      setError(null);
      setPreflighting(true);
      void openFile(expectedEditorFile).then(startNavigation, (cause: unknown) => {
        if (preflightRequestRef.current !== requestID) return;
        setPreflighting(false);
        setError(botPreflightError(expectedEditorFile, cause));
      });
    },
    [setLocation],
  );

  useEffect(() => {
    if (!pending) return;
    let stopped = false;
    let timer: number | undefined;
    const targetPath = pending.href.split("?")[0] || "/";
    const expectedEditorFile = pending.targetRef.startsWith("bot/")
      ? pending.targetRef.slice("bot/".length)
      : null;

    const fail = (message: string) => {
      if (stopped) return;
      setError(message);
      setPending(null);
    };

    const finish = async () => {
      if (sendingRef.current || stopped) return;
      sendingRef.current = true;
      try {
        // Choosing a navigate_to reply is an explicit one-message attachment,
        // not permission to replace the conversation's immutable anchor.
        await sendRef.current(pending.message, pending.targetRef);
        if (!stopped) setPending(null);
      } catch (cause) {
        fail(
          cause instanceof Error
            ? cause.message
            : "The assistant reply could not be sent.",
        );
      } finally {
        sendingRef.current = false;
      }
    };

    const check = async () => {
      if (stopped) return;
      if (Date.now() - pending.startedAt >= NAVIGATION_REPLY_TTL_MS) {
        fail(
          "The destination did not finish loading. Nothing was sent to the assistant.",
        );
        return;
      }

      if (route === targetPath) {
        if (expectedEditorFile) {
          try {
            const snapshot = await captureActiveEditorDocument();
            if (snapshot?.file === expectedEditorFile) {
              // An oversized document is WITHHELD, not a failure to send.
              // captureActiveEditorDocument already drops `source` and leaves
              // `complete: false` + `sourceLength` in the snapshot, and
              // withActiveEditorDocument ships that marker — which is exactly
              // what the capture layer's own comment promises ("the marker
              // tells the bot the document was withheld").
              //
              // Failing here contradicted that on the one path where the
              // operator has ALREADY expressed an intent by clicking. They got
              // a dead click and the assistant never learned what was asked;
              // meanwhile the ordinary composer send, carrying the same
              // oversized document, goes through with the marker. Same
              // situation, opposite outcome.
              //
              // Sending the marker lets the bot answer usefully — name the
              // file, say it is too large to read inline, ask which part, or
              // reach for its own read tools. Still never a prefix: a partial
              // workflow looks editable and cannot be validated honestly.
              await finish();
              return;
            }
          } catch {
            // The editor is still hydrating. Retry until the bounded timeout;
            // transient parse/unparse failures must not send a context-less
            // modification request.
          }
        } else {
          await finish();
          return;
        }
      }
      timer = window.setTimeout(() => void check(), 50);
    };

    void check();
    return () => {
      stopped = true;
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [pending, route]);

  return { submit, busy: preflighting || pending !== null, error };
}
