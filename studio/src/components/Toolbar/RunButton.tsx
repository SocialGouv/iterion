import { ExclamationTriangleIcon, PlayIcon } from "@radix-ui/react-icons";
import { useLocation } from "wouter";

import { Button, IconButton } from "@/components/ui";
import { launchRefusal } from "@/lib/launch";
import { useBackendDetectStore } from "@/store/backendDetect";
import { useDocumentStore } from "@/store/document";

// The toolbar's Run: launches the bound file by path, or an unsaved buffer
// inline (the only launch cloud has, where the pod rootfs is read-only and a
// workflow can never be saved to disk). Enabled exactly when the launch view
// would launch — it reads the same `launchRefusal` — so the button never
// leads to an empty picker or to a launch the server refuses: a fresh
// buffer, a salvage and a document with errors disable it, and its title
// says which.
export default function RunButton() {
  const [, setLocation] = useLocation();
  const currentFilePath = useDocumentStore((s) => s.currentFilePath);
  const refusal = useDocumentStore((s) => launchRefusal(s));
  const hasResolvedBackend = useBackendDetectStore((s) => !!s.report?.resolved_default);
  // `report != null` once the host probe has returned (success or fail) —
  // gates the missing-credential nudge so it doesn't flash during the boot probe.
  const backendProbed = useBackendDetectStore((s) => s.report != null);
  return (
    <>
      {backendProbed && !hasResolvedBackend && !refusal && (
        // The credential is the only thing holding this launch: surface a
        // clickable nudge straight to Preferences → Backends — otherwise the
        // only signal is a silently greyed-out button.
        <IconButton
          variant="warning"
          size="sm"
          label="No LLM credential detected — open Preferences → Backends"
          tooltip="No LLM credential detected — click to open Preferences → Backends"
          onClick={() =>
            window.dispatchEvent(
              new CustomEvent("iterion:open-settings", {
                detail: { tab: "backends" },
              }),
            )
          }
        >
          <ExclamationTriangleIcon />
        </IconButton>
      )}
      <Button
        variant="primary"
        size="sm"
        leadingIcon={<PlayIcon />}
        onClick={() =>
          setLocation(
            currentFilePath
              ? `/runs/new?file=${encodeURIComponent(currentFilePath)}`
              : `/runs/new`,
          )
        }
        disabled={!!refusal || !hasResolvedBackend}
        title={
          refusal ??
          (!hasResolvedBackend
            ? "No LLM credentials detected — open Preferences → Backends to configure."
            : currentFilePath
              ? `Launch ${currentFilePath}`
              : "Launch the unsaved workflow")
        }
      >
        Run
      </Button>
    </>
  );
}
