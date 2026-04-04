import { Position } from "@xyflow/react";

export const SIDES = ["top", "right", "bottom", "left"] as const;
export const POS_MAP: Record<typeof SIDES[number], Position> = {
  top: Position.Top,
  right: Position.Right,
  bottom: Position.Bottom,
  left: Position.Left,
};
