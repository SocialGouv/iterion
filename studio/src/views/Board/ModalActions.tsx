// Shared Cancel + primary footer for the board's management dialogs
// (column add/edit/delete, field add/edit, save-view). One place for the
// busy/disabled wiring so the three dialog families stay consistent.

import { Button } from "@/components/ui/Button";

export function ModalActions({
  onCancel,
  primaryLabel,
  primaryVariant = "primary",
  onPrimary,
  busy,
  disabled,
}: {
  onCancel: () => void;
  primaryLabel: string;
  primaryVariant?: "primary" | "danger";
  onPrimary: () => void;
  busy: boolean;
  disabled?: boolean;
}) {
  return (
    <>
      <Button variant="secondary" size="sm" onClick={onCancel} disabled={busy}>
        Cancel
      </Button>
      <Button
        variant={primaryVariant}
        size="sm"
        onClick={onPrimary}
        loading={busy}
        disabled={busy || disabled}
      >
        {busy ? "…" : primaryLabel}
      </Button>
    </>
  );
}
