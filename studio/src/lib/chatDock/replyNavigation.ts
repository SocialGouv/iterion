// A suggested assistant reply may require another Studio surface.
//
// The model supplies a typed reference, never an href. The Studio resolves it,
// navigates, waits for the destination context, and only then sends the
// operator's chosen message.

import { useCallback, useEffect, useRef, useState } from "react";
import { useLocation } from "wouter";

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
  send: (message: string) => Promise<unknown>,
): NavigationReplyController {
  const [route, setLocation] = useLocation();
  const [pending, setPending] = useState<PendingNavigationReply | null>(null);
  const [error, setError] = useState<string | null>(null);
  const sendingRef = useRef(false);
  const sendRef = useRef(send);
  useEffect(() => {
    sendRef.current = send;
  }, [send]);

  const submit = useCallback(
    (message: string, targetRef: string) => {
      const href = hrefForAssistantReplyTarget(targetRef);
      if (!href) {
        setError("This assistant destination is not available in the Studio.");
        return;
      }
      setError(null);
      setPending({
        message,
        targetRef,
        href,
        startedAt: Date.now(),
      });
      setLocation(href);
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
        await sendRef.current(pending.message);
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
              if (!snapshot.complete) {
                fail(
                  "The editor document is too large to send completely. Nothing was sent to the assistant.",
                );
                return;
              }
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

  return { submit, busy: pending !== null, error };
}
