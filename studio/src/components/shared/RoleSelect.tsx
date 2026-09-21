// RoleSelect — pick a role from a ladder, and confirm the changes that can
// lock someone out or hand over control.
//
// The prompt travels WITH the select rather than living in each page: the
// super-admin drawer reaches any org or team on the platform, so a surface
// that forgot to wrap its own write could hand over ownership of a tenant
// in one un-prompted click. Mounted without `confirmChangeFrom` it is a
// plain select — the right shape for "which role am I about to grant?",
// where nothing is being taken away yet.

import { Select } from "@/components/ui/Select";
import type { Confirmer } from "@/hooks/useConfirm";
import { needsRoleChangeConfirm, roleLabel } from "@/lib/roles";

export function RoleSelect({
  value,
  roles,
  ariaLabel,
  id,
  size,
  disabled = false,
  confirmChangeFrom,
  confirm,
  onChange,
}: {
  value: string;
  roles: readonly string[];
  ariaLabel?: string;
  id?: string;
  size?: "sm" | "md";
  disabled?: boolean;
  /**
   * The role currently held. When set, a demotion or anything touching
   * `owner` is confirmed before `onChange` fires. Leave unset when the
   * select is choosing a role to GRANT rather than changing one.
   */
  confirmChangeFrom?: string;
  confirm?: Confirmer;
  onChange: (role: string) => unknown;
}) {
  const handle = async (next: string) => {
    if (confirmChangeFrom != null && confirm) {
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
