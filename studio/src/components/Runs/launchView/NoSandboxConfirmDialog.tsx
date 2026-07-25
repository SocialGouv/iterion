// Extracted from LaunchView.tsx to keep that file focused.
// Confirmation shown when the user clicks Launch on a workflow that
// EXPLICITLY opts out of sandboxing (sandbox: none) — sandboxing is the
// default, so an absent block never triggers this; only a declared
// opt-out does, and host (or bare runner pod) execution carries real
// risk, so the choice must be deliberate.

import ConfirmDialog from "@/components/shared/ConfirmDialog";

export interface NoSandboxConfirmDialogProps {
  open: boolean;
  cloud: boolean;
  onConfirm: () => void;
  onCancel: () => void;
  onEditWorkflow: () => void;
}

export default function NoSandboxConfirmDialog({
  open,
  cloud,
  onConfirm,
  onCancel,
  onEditWorkflow,
}: NoSandboxConfirmDialogProps) {
  return (
    <ConfirmDialog
      open={open}
      title="Launch without sandbox?"
      message={
        cloud ? (
          <>
            <p>
              This workflow <strong>explicitly opts out</strong> of
              sandboxing (<code>sandbox: none</code>), so its tools and
              shell commands run directly in the runner pod's
              filesystem — no container isolation between the bot and
              the runner's mounted credentials, workspace clone, or
              outbound network egress.
            </p>
            <p>
              Sandboxing is the default: remove the{" "}
              <code>sandbox: none</code> block to run sandboxed. Keep
              the opt-out only if this flow genuinely needs the
              runner's own environment.
            </p>
          </>
        ) : (
          <>
            <p>
              This workflow <strong>explicitly opts out</strong> of
              sandboxing (<code>sandbox: none</code>), so its tools and
              shell commands will run directly on the host. The bot can
              read, modify, or delete any file the iterion process has
              access to.
            </p>
            <p>
              Sandboxing is the default: remove the{" "}
              <code>sandbox: none</code> block to run sandboxed. Keep
              the opt-out only if this flow genuinely needs the host.
            </p>
          </>
        )
      }
      confirmLabel="Launch unsandboxed"
      confirmVariant="danger"
      secondaryAction={{
        label: "Edit workflow first",
        onClick: onEditWorkflow,
      }}
      onConfirm={onConfirm}
      onCancel={onCancel}
    />
  );
}
