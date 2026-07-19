// Extracted from LaunchView.tsx to keep that file focused.
// Confirmation shown when the user clicks Launch on a workflow with no
// sandbox active — host (or bare runner pod) execution carries real
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
              This workflow doesn't declare a <code>sandbox:</code>{" "}
              block, so its tools and shell commands run directly in
              the ephemeral runner pod's filesystem — no container
              isolation between the bot and the runner's mounted
              credentials, workspace clone, or outbound network egress.
            </p>
            <p>
              Add <code>sandbox: auto</code> (devcontainer-aware) or
              an inline block with an image in the workflow file to
              narrow the write scope and the tools the bot can reach.
            </p>
          </>
        ) : (
          <>
            <p>
              This workflow doesn't declare a <code>sandbox:</code>{" "}
              block, so its tools and shell commands will run directly
              on the host. The bot can read, modify, or delete any
              file the iterion process has access to.
            </p>
            <p>
              Add <code>sandbox: auto</code> (devcontainer-aware) or
              an inline block with an image in the workflow file to
              opt into container isolation.
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
