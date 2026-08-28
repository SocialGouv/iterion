import {
  useCallback,
  useEffect,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import { CheckIcon, ExclamationTriangleIcon } from "@radix-ui/react-icons";
import { useLocation } from "wouter";

import {
  commitAssistantAuthoring,
  previewAssistantAuthoring,
  type AssistantAuthoringPreviewFile,
} from "@/api/assistantAuthoring";
import { Button } from "@/components/ui/Button";
import { useFileChangeProposal } from "@/hooks/useFileChangeProposal";
import {
  decideAssistantAction,
  useAssistantActionPolicy,
} from "@/lib/chatDock/assistantActions";
import {
  isEditorSessionActive,
  resolveAuthoringSnapshot,
  resolveEditorSession,
} from "@/lib/chatDock/editorSession";
import { useUIStore } from "@/store/ui";

import AssistantTextDiffDialog from "./AssistantTextDiffDialog";

type State = "idle" | "previewing" | "ready" | "saving" | "saved" | "error";

export default function AssistantFileChangeOffer({
  runId,
  revision,
}: {
  runId: string | null;
  revision: number;
}) {
  const proposal = useFileChangeProposal(runId, revision);
  const [route] = useLocation();
  const addToast = useUIStore((state) => state.addToast);
  const policy = useAssistantActionPolicy("editor.files.save");
  const decision = decideAssistantAction(policy, proposal.intent === "explicit");
  const [state, setState] = useState<State>("idle");
  const [error, setError] = useState<string | null>(null);
  const [preview, setPreview] = useState<AssistantAuthoringPreviewFile[]>([]);
  const [selected, setSelected] = useState<AssistantAuthoringPreviewFile | null>(null);
  const autoStarted = useRef<string | null>(null);

  const session = proposal.sessionId
    ? resolveEditorSession(proposal.sessionId)
    : null;
  const snapshot =
    proposal.sessionId && proposal.revision !== null
      ? resolveAuthoringSnapshot(proposal.sessionId, proposal.revision)
      : null;
  const subscribeRevision = useCallback(
    (notify: () => void) => session?.store.subscribe(notify) ?? (() => {}),
    [session?.store],
  );
  const readRevision = useCallback(
    () => session?.store.getState()._generation ?? null,
    [session?.store],
  );
  const currentRevision = useSyncExternalStore(
    subscribeRevision,
    readRevision,
    () => null,
  );
  const unavailable =
    !proposal.sessionId ||
    proposal.revision === null ||
    !route.startsWith("/editor") ||
    !isEditorSessionActive(proposal.sessionId) ||
    currentRevision !== proposal.revision ||
    !snapshot;

  const review = useCallback(async () => {
    if (!snapshot || proposal.changes.length === 0 || unavailable) {
      setError("The editor tab or revision changed. Ask Copi again from the open bot.");
      setState("error");
      return;
    }
    setState("previewing");
    setError(null);
    try {
      const result = await previewAssistantAuthoring(snapshot, proposal.changes);
      setPreview(result.files);
      setSelected(result.files[0] ?? null);
      setState("ready");
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not preview the changes");
      setState("error");
    }
  }, [snapshot, proposal.changes, unavailable]);

  const save = useCallback(async () => {
    if (!snapshot || proposal.changes.length === 0 || unavailable) return;
    setState("saving");
    setError(null);
    try {
      const result = await commitAssistantAuthoring(snapshot, proposal.changes);
      setPreview(result.files);
      setState("saved");
      addToast(
        `${result.files.length} assistant file change${result.files.length === 1 ? "" : "s"} saved`,
        "success",
      );
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not save the changes");
      setState("error");
    }
  }, [snapshot, proposal.changes, unavailable, addToast]);

  useEffect(() => {
    if (
      decision !== "auto" ||
      unavailable ||
      state !== "idle" ||
      !proposal.sessionId ||
      proposal.revision === null
    ) {
      return;
    }
    const key = `${proposal.sessionId}:${proposal.revision}`;
    if (autoStarted.current === key) return;
    autoStarted.current = key;
    void save();
  }, [decision, unavailable, state, proposal.sessionId, proposal.revision, save]);

  if (proposal.changes.length === 0 || !proposal.sessionId || proposal.revision === null) {
    return null;
  }

  let detail = `${proposal.changes.length} declared companion file${proposal.changes.length === 1 ? "" : "s"}. Scripts are not verified by tests in v1.`;
  if (!snapshot) detail = "The authoring snapshot for this editor turn is no longer available. Ask Copi again from the open bot.";
  else if (unavailable) detail = "Return to the unchanged editor tab before reviewing or saving these files.";
  else if (decision === "deny") detail = "Companion-file saves are disabled in Settings → Assistant.";
  else if (state === "previewing") detail = "Resolving exact replacements and compiling changed bot files…";
  else if (state === "ready") detail = "Preview ready. Review any file, then confirm the save.";
  else if (state === "saving") detail = "Rechecking hashes and saving the reviewed files…";
  else if (state === "saved") detail = "Files saved after fresh hash and compile checks. Script tests were not run.";

  return (
    <div className="mt-3 rounded-md border border-border-subtle bg-surface-2 p-2.5">
      <div className="flex items-start gap-2">
        {state === "saved" ? (
          <CheckIcon className="mt-0.5 h-4 w-4 text-success-fg" aria-hidden="true" />
        ) : (
          <ExclamationTriangleIcon className="mt-0.5 h-4 w-4 text-accent-text" aria-hidden="true" />
        )}
        <div className="min-w-0 flex-1">
          <p className="text-label font-medium">
            {state === "saved" ? "Companion files saved" : "Proposed companion-file changes"}
          </p>
          <p className="mt-0.5 text-caption text-fg-muted">{detail}</p>
          {error && <p className="mt-1 text-caption text-danger-fg">{error}</p>}
          {preview.length > 0 && (
            <div className="mt-2 flex flex-wrap gap-1.5">
              {preview.map((file) => (
                <Button
                  key={`${file.scope}:${file.path}`}
                  variant="secondary"
                  size="sm"
                  onClick={() => setSelected(file)}
                >
                  {file.path}
                </Button>
              ))}
            </div>
          )}
          <div className="mt-2 flex flex-wrap gap-2">
            {state !== "saved" && decision !== "deny" && (
              <Button
                variant="secondary"
                size="sm"
                disabled={unavailable || state === "previewing" || state === "saving"}
                onClick={() => void review()}
              >
                {state === "previewing" ? "Preparing preview…" : "Review changes"}
              </Button>
            )}
            {state === "ready" && decision === "confirm" && (
              <Button
                variant="primary"
                size="sm"
                disabled={unavailable}
                onClick={() => void save()}
              >
                Save declared files
              </Button>
            )}
          </div>
        </div>
      </div>
      <AssistantTextDiffDialog file={selected} onClose={() => setSelected(null)} />
    </div>
  );
}
