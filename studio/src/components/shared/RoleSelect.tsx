// RoleSelect — pick a role from a ladder, and confirm the changes that can
// lock someone out or hand over control.
//
// The prompt travels WITH the select rather than living in each page: the
// super-admin drawer reaches any org or team on the platform, so a surface
// that forgot to wrap its own write could hand over ownership of a tenant in
// one un-prompted click.
//
// It guards a CHANGE only — a select that IS the write. Pass
// `confirmChangeFrom` with the role currently held, and a demotion, a move
// across `config_editor`, or anything touching `owner` is confirmed first.
//
// A select that merely picks a role to GRANT is a plain select here: the
// write happens at a button further down the form, and a prompt on the
// dropdown would guard a gesture that writes nothing — measured, it also
// let the SECOND grant through unprompted, because the form keeps its role.
// That guard lives at the write, in `confirmOwnerGrant`.

import { Select } from "@/components/ui/Select";
import type { Confirmer } from "@/hooks/useConfirm";
import { needsRoleChangeConfirm, roleLabel } from "@/lib/roles";

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
  | { confirmChangeFrom?: undefined; confirm?: undefined };

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
