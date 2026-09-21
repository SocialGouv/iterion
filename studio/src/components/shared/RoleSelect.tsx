// RoleSelect — pick a role from a ladder, and confirm the changes that can
// lock someone out or hand over control.
//
// The prompt travels WITH the select rather than living in each page: the
// super-admin drawer reaches any org or team on the platform, so a surface
// that forgot to wrap its own write could hand over ownership of a tenant in
// one un-prompted click.
//
// Two modes, and the type makes the second impossible to half-configure:
//   - CHANGE (`confirmChangeFrom` + `confirm`): a demotion, a move across
//     `config_editor`, or anything touching `owner` is confirmed first.
//   - GRANT (`confirm` alone, or neither): nothing is being taken away, so
//     only the `owner` half applies — installing an owner hands over control
//     just as much as promoting one.

import { Select } from "@/components/ui/Select";
import type { Confirmer } from "@/hooks/useConfirm";
import { needsRoleChangeConfirm, needsRoleGrantConfirm, roleLabel } from "@/lib/roles";

interface BaseProps {
  value: string;
  roles: readonly string[];
  ariaLabel?: string;
  id?: string;
  size?: "sm" | "md";
  disabled?: boolean;
  onChange: (role: string) => unknown;
}

// Naming a current role REQUIRES a confirmer. Typed as a union rather than
// two optional props so a caller cannot ask for the guard and silently not
// get it — the guard's whole purpose is that forgetting is not one prop away.
type ConfirmProps =
  | { confirmChangeFrom: string; confirm: Confirmer }
  | { confirmChangeFrom?: undefined; confirm?: Confirmer };

export function RoleSelect(props: BaseProps & ConfirmProps) {
  const {
    value,
    roles,
    ariaLabel,
    id,
    size,
    disabled = false,
    confirmChangeFrom,
    confirm,
    onChange,
  } = props;

  const handle = async (next: string) => {
    if (confirmChangeFrom != null) {
      if (next === confirmChangeFrom) return;
      if (needsRoleChangeConfirm(confirmChangeFrom, next, roles)) {
        const ok = await confirm({
          title: "Change role?",
          message: `Change this member from "${roleLabel(confirmChangeFrom)}" to "${roleLabel(next)}"? This takes effect immediately.`,
          confirmLabel: "Change role",
          confirmVariant: "danger",
        });
        // On cancel the controlled <Select> re-renders back to the stored
        // role, so there is nothing to revert by hand.
        if (!ok) return;
      }
    } else if (confirm && needsRoleGrantConfirm(next)) {
      const ok = await confirm({
        title: "Grant ownership?",
        message: `"${roleLabel(next)}" hands over control of this tenant. Grant it?`,
        confirmLabel: "Grant owner",
        confirmVariant: "danger",
      });
      if (!ok) return;
    }
    await onChange(next);
  };

  return (
    <Select
      id={id}
      size={size}
      value={value}
      disabled={disabled}
      aria-label={ariaLabel}
      onChange={(e) => void handle(e.target.value)}
    >
      {roles.map((r) => (
        <option key={r} value={r}>
          {roleLabel(r)}
        </option>
      ))}
    </Select>
  );
}
