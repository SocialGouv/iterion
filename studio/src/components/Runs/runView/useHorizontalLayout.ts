import { useLayoutPersistence } from "@/hooks/useLayoutPersistence";

// Owns the horizontal-layout persistence for the canvas row (canvas /
// detail / optional Side dock) and derives the active handle + per-Panel
// size baselines based on whether the right-hand Side dock (Chat +
// Browser tabs) is mounted. Lifted out of RunView so the host doesn't
// repeat the ternary at every {defaultLayout, onLayoutChanged,
// defaultSize} site.
//
// Chat and Browser used to be two independent right-dock Panels (up to
// four columns: canvas|detail|browser|chat). They now share one tabbed
// Side dock, so a single "side open?" bit picks the layout — no more
// 4-way dock-combo matrix.
export interface HorizontalLayoutResult {
  active: ReturnType<typeof useLayoutPersistence>;
  canvasSize: number;
  detailSize: number;
  sideSize: number;
  // Reset both persistence handles so a host-level "Reset layout" snaps
  // the horizontal Group back to default regardless of the active combo.
  resetAll: () => void;
}

export function useHorizontalLayout({
  sideDockOpen,
}: {
  sideDockOpen: boolean;
}): HorizontalLayoutResult {
  const horizontalLayout = useLayoutPersistence("run-console-v2.horizontal", {
    canvas: 70,
    detail: 30,
  });
  // Separate layout key so the canvas / detail / side split doesn't
  // collide with the canvas/detail-only layout when the dock toggles.
  const horizontalLayoutWithSide = useLayoutPersistence(
    "run-console-v2.horizontal-with-side",
    { canvas: 50, detail: 22, side: 28 },
  );

  const active = sideDockOpen ? horizontalLayoutWithSide : horizontalLayout;
  const canvasSize = sideDockOpen ? 50 : 70;
  const detailSize = sideDockOpen ? 22 : 30;
  const sideSize = 28;

  const resetAll = () => {
    // Each layout's reset() bumps its own groupKey, remounting the Groups
    // so they re-read the just-reset defaultLayout — see
    // useLayoutPersistence.
    horizontalLayout.reset();
    horizontalLayoutWithSide.reset();
  };

  return { active, canvasSize, detailSize, sideSize, resetAll };
}
