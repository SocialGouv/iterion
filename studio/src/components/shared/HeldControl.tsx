// A control held for a reason its user must be able to reach. A natively
// disabled button takes no focus, so a reason in its `title` exists for a
// mouse alone. Held here, the control stays in the tab order and says it is
// unavailable (aria-disabled); its description is the reason — a node that is
// always in the tree, so it is announced whether or not the tooltip is open —
// and the tooltip shows the same sentence on hover and on focus. The control's
// own handler does nothing while it is held: that stays the caller's rule.
// The control is one element at one place whether held or not, so a hold that
// lifts while it has the focus leaves the focus on it.

import { cloneElement, useId, type ReactElement } from "react";

import { Tooltip } from "@/components/ui";

interface HeldProps {
  "aria-disabled"?: boolean;
  "aria-describedby"?: string;
  className?: string;
}

export default function HeldControl({
  reason,
  children,
}: {
  /** Why the control is held; null renders it as it is. */
  reason: string | null;
  children: ReactElement<HeldProps>;
}) {
  const id = useId();
  // One tree shape whatever the reason: the control keeps its place — and the
  // keyboard focus it may hold — when the hold lifts or falls.
  return (
    <>
      <Tooltip content={reason} stable>
        {reason
          ? cloneElement(children, {
              "aria-disabled": true,
              "aria-describedby": id,
              className: [children.props.className, "opacity-50 cursor-not-allowed"]
                .filter(Boolean)
                .join(" "),
            })
          : children}
      </Tooltip>
      {reason && (
        <span id={id} className="sr-only">
          {reason}
        </span>
      )}
    </>
  );
}
